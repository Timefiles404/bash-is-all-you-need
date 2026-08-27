// 阶段 02——trace 文件。
//
// 第一个不是渲染器的订阅者。它什么都不画；它把事件流变成文件，而下游的一
// 切都靠这个文件才成立：不用 API key 就能重放、下周还能重跑一遍的成本
// 报表、拿得出证据而不是靠回忆某段滚屏内容的 bug 报告。
//
// 格式选 JSONL——每行一个 JSON 对象——压倒一切的理由只有一条：这是唯一一
// 种写到一半被打断只赔掉最后一条记录、而不是整个文件的文本格式。JSON 数组
// 需要一个收尾的方括号，而被杀掉的进程永远写不出它，于是那份记录崩溃的文
// 件会*因为*这次崩溃而没法解析。replay.go 里的 ReadTrace 是这笔交易的另一
// 半。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// TraceWriter 往文件里一行一个事件地追加。它是个 Subscriber，所以 Agent 核
// 心从头到尾都不知道它存在。
type TraceWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File

	closed bool

	// err 只装*第一次*写失败，之后的一概不装。每次失败都报的写入器，会把"磁
	// 盘满了"变成一万行噪音，糊在用户本来想用来看 Agent 的那个终端上。失败只
	// 吵一次；之后记录就安静地降级，由 Close 用一个数字报出损失。
	err     error
	dropped int

	// 那唯一一次提示就从 warn 出去。做成字段，是为了让测试能断言"只报一次"里
	// 的"一次"，而不用把测试 runner 的 stderr 喷得到处都是。
	warn func(format string, args ...any)
}

// NewTraceWriter 打开 path，以追加方式一行写一个 JSON 对象。
func NewTraceWriter(path string) (*TraceWriter, error) {
	// 真实的 trace 住在按日期分的目录里
	// （traces/2026-08-27/session-3.jsonl），所以建父目录是这份活儿的一部分，
	// 而不是每个调用方的杂事。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("trace: cannot create %s: %w", dir, err)
		}
	}

	// 用 O_APPEND，不用 O_TRUNC：续上的会话是往自己的 trace 后面接，而不是把
	// 它删掉；而在 O_APPEND 下，每次写都作为一个操作落在文件当前的末尾——于
	// 是两个 Agent 指着同一份 trace 时，交错的是整行，而不是互相盖掉对方的偏
	// 移。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("trace: cannot open %s: %w", path, err)
	}
	return &TraceWriter{
		path: path,
		f:    f,
		warn: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}, nil
}

// Path 是 trace 正在往哪儿写，这样会话结束时渲染器能告诉用户去哪儿找它。
func (w *TraceWriter) Path() string { return w.path }

// OnEvent 记录一个事件。它不会以任何调用方能观察到的方式失败，这是有意如
// 此。
//
// Bus.Emit 是握着自己的锁同步分发的。这里面 panic，崩的不是"trace"——崩的
// 是 Agent，在回合中间，带着一份流到一半的回复和一个没回收的子进程。这个
// 文件能搞错的任何事都不值这个代价，所以整个方法就是一道兜底：吞下、记
// 下、继续走。吞掉错误通常是 bug；可订阅者跑在别的组件的锁里面，在这儿它
// 就是契约。
func (w *TraceWriter) OnEvent(e Event) {
	defer func() {
		// 只有不可能的事真发生了才会走到这儿——将来某个字段的 MarshalJSON
		// 会 panic，或者某次重构搞砸之后 *os.File 成了 nil。recover 是在
		// writeEvent 的 deferred Unlock 已经跑完之后才执行的，所以 fail 可
		// 以再拿一次锁而不会死锁。
		if r := recover(); r != nil {
			w.fail(fmt.Errorf("panic writing event %d (%s): %v", e.Seq, e.Kind, r))
		}
	}()
	w.writeEvent(e)
}

func (w *TraceWriter) writeEvent(e Event) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed || w.err != nil {
		// 已经降级了。计个数，好让 Close 能说出这次会话缺了多少：一份悄没
		// 声就短了一截的 trace，比根本没有 trace 更糟，因为它看着是完整的。
		w.dropped++
		return
	}

	line, err := marshalEvent(e)
	if err != nil {
		// 实际上这意味着 Request 里装的字节不是合法 JSON——比方说逐字抓下
		// 来的供应商 body。把载荷丢掉，把事件留下：少了一份请求 body 的
		// trace 仍然是 trace，而 Seq 序列上破个洞，半年后没人解得开这个谜。
		degraded := e
		degraded.Request = json.RawMessage(`{"trace_error":"request body was not valid JSON and was dropped"}`)
		line, err = marshalEvent(degraded)
		if err != nil {
			w.failLocked(fmt.Errorf("encode event %d (%s): %w", e.Seq, e.Kind, err))
			return
		}
	}
	line = append(line, '\n')

	// 持久性。字节直接落到文件里：这条路径上没有 bufio.Writer，而它的缺席就
	// 是整个设计。
	//
	// 64KB 的缓冲会把几百个事件攒成一次系统调用，然后在 Agent 被杀掉的时候把
	// 它们全部丢光——而 trace 存在的意义，恰恰就是解释那一刻。不带缓冲的代价是
	// 每个事件一次 write(2)，几微秒进内核页缓存；对面是以几百毫秒计的模型调
	// 用。这笔账根本不用算。
	//
	// 刻意停在 f.Sync() 之前。fsync 额外扛得住断电，代价是真实的磁盘延迟
	// （SSD 上约 0.1ms，机械盘或网络挂载上约 10ms），而且是在*每一条文本
	// delta* 上、在总线锁里面付：贵三个数量级，换来的是防住一种（机器挂掉）
	// 比已经防住的那种（进程挂掉）罕见得多的故障。这个 Write 一返回，数据就
	// 扛得住 SIGKILL、panic 和 os.Exit，不必我们再帮什么忙。
	//
	// 每行一次 Write，也让一行在 O_APPEND 下保持原子——正是这一点挡住了并发
	// 写入者把一条解析不了的记录拼插进文件中间。
	if _, err := w.f.Write(line); err != nil {
		w.failLocked(fmt.Errorf("write to %s: %w", w.path, err))
	}
}

// 注意这里*没有*什么：没有 goroutine，没有 channel，没有队列。
//
// "绝不要卡住总线"通常的答案是写个异步写入器，而队列满了之后只有两种行
// 为——要么卡住生产者（正是要躲开的那件事），要么丢事件（一份靠省略来撒
// 谎的 trace，悄无声息，而且恰恰是在你最想记录下来的那种负载下撒）。本地
// 追加从来不会无界地等，所以同步版本这两个毛病都没有。总线真正需要的规矩
// 是"不能无界等待"，不是"不能做 I/O"：不 fsync，不走网络，不在 channel 发
// 送上持着锁。

func (w *TraceWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failLocked(err)
}

func (w *TraceWriter) failLocked(err error) {
	w.dropped++
	if w.err != nil {
		return // 已经报过一次了；闭嘴，接着计数
	}
	w.err = err
	if w.warn != nil {
		w.warn("trace: %v — recording is disabled for the rest of this session", err)
	}
}

// Close 什么都不 flush（本来就没缓冲），它报的是损失。
//
// 它是幂等的，因为 main 里 defer 了它，而信号处理函数也可能再调一次；第二次
// Close 要是返回错误，一次有序的关停就会看着像失败。
func (w *TraceWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true

	cerr := w.f.Close()
	if w.err != nil {
		return fmt.Errorf("trace %s: %d event(s) went unrecorded after the first failure: %w",
			w.path, w.dropped, w.err)
	}
	return cerr
}

// marshalEvent 编码一个事件，HTML 转义**关掉**。
//
// json.Marshal 会把 <、> 和 & 转成 \u003c、\u003e 和 \u0026，而且——这一条
// 才咬人——encoding/json 压缩 json.RawMessage 时，在它*内部*也照样这么干。
// Event.Request 就是个 RawMessage，装着适配器发出去的那些确切字节，而两个
// 适配器都特意用 SetEscapeHTML(false) 来编码，正是因为 shell Agent 的请求
// 里满是 `2>&1`、`>/tmp/out` 和 `<<EOF`。
//
// 所以没有这一手，适配器小心翼翼护住的一切，就在下一层、在文件里被推翻
// 了：
//
//	发出的：  {"command":"ls 2>&1 <in"}
//	记下的：  {"command":"ls 2\u003e\u00261 \u003cin"}
//
// 没有任何报错，JSON 是等价的，每个解码它的消费方拿回的都是对的字符串。坏
// 掉的是那句承诺：events.go 管 Request 叫"即将发出去的确切字节"，阶段 06
// 的线上视角保证"逐字节一致"，而把一次实跑和一次重放做字节级对比，看到的
// diff 全是这个。trace 是证据；它一旦不再逐字节相同，就不再是关于字节的证
// 据了。
//
// 是在一份真实 trace 里发现的，那里记下的 24 个请求全都带着这些转义。
func marshalEvent(e Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	// Encoder.Encode 会补一个换行，Marshal 不会；调用方自己还会加一个，两个
	// 换行就是 JSONL 文件中间的一个空行。
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
