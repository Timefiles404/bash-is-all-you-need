// 阶段 02——trace 文件。
//
// 第一个不是渲染器的订阅者。它什么都不画；它把事件流变成一个
// 文件，正是这个文件，让下游的一切成为可能：免 API 密钥的重放、
// 一份下周还能重新跑一遍的成本报告、一份是证据而不是模糊
// scrollback 记忆的 bug 报告。
//
// 格式是 JSONL——每行一个 JSON 对象——最重要的一个原因是：它是
// 唯一一种在写入过程中被打断时，只让你丢掉最后一条记录、而不是
// 丢掉整个文件的文本格式。JSON 数组需要一个收尾的括号，而一个
// 被杀死的进程永远不会写下这个括号，所以记录这次崩溃的文件，会
// **因为**这次崩溃本身而变得无法解析。replay.go 里的 ReadTrace，
// 是这份约定的另一半。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// TraceWriter 把每个事件追加一行，写进一个文件。它是一个
// Subscriber，所以 Agent 核心永远不会知道它的存在。
type TraceWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File

	closed bool

	// err 只保留**第一次**写失败，之后就什么都不再记了。如果写入器
	// 把每一次失败都上报，磁盘一满，就会在用户原本想用来看 Agent
	// 输出的终端上，刷出一万行噪音。这次失败只会高调地出现一次；
	// 在那之后，记录会悄悄地跟着退化，Close 则用一个数字来报告
	// 损失了多少。
	err     error
	dropped int

	// warn 就是那一条唯一通知的去处。它是一个字段，这样测试就能
	// 断言"记录一次"里的"一次"，又不会把测试运行者的 stderr 喷得
	// 到处都是。
	warn func(format string, args ...any)
}

// NewTraceWriter 打开 path，用来以追加方式写入，每行一个 JSON 对象。
func NewTraceWriter(path string) (*TraceWriter, error) {
	// 真实的 trace 存放在按日期分的目录里（traces/2026-08-27/session-3.jsonl），
	// 所以创建父目录是这个函数分内的工作，而不是每个调用者自己的
	// 麻烦事。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("trace: cannot create %s: %w", dir, err)
		}
	}

	// O_APPEND，不是 O_TRUNC：一个恢复的会话扩展它自己的 trace 而
	// 不是删除它；在 O_APPEND 下，每个写都会作为一次单独的操作，
	// 落在文件当前的末尾——所以两个 Agent 指向同一个 trace，会交错
	// 着写下完整的行，而不是覆盖彼此的偏移。
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

// Path 是 trace 被写入的位置，这样会话结束时，渲染器就能告诉
// 用户去哪里找它。
func (w *TraceWriter) Path() string { return w.path }

// OnEvent 记录一个事件。它不能以调用者能观察到的任何方式失败，
// 这是故意的。
//
// Bus.Emit 在持有自己锁的同时同步分发。这里的一个 panic，垮掉的
// 不是"trace"——垮掉的是 Agent 本身，就在回合中途，还带着一条
// 流到一半的回复和一个没回收的子进程。这个文件可能出的任何错，
// 都不值得付出那样的代价，所以整个方法是一道兜底：它吞，它记录，
// 它继续。吞掉错误通常算是一个 bug；但对一个运行在别的组件锁
// 内部的订阅者来说，这就是契约。
func (w *TraceWriter) OnEvent(e Event) {
	defer func() {
		// 只有不可能发生的事情发生了，才会走到这里——比如一个未来
		// 新增、MarshalJSON 会 panic 的字段，或者一次搞砸的重构之后
		// 留下的 nil *os.File。recover 是在 writeEvent 延迟执行的 Unlock
		// 已经触发之后才运行的，所以 fail 可以再次获取锁，而不会死锁。
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
		// 已经降级。计数它，这样 Close 就能说清楚缺了多少会话内容：
		// 一份悄悄变短、自己不吭声的 trace，比完全没有 trace 还要糟糕，
		// 因为它看起来是完整的。
		w.dropped++
		return
	}

	line, err := marshalEvent(e)
	if err != nil {
		// 实践中这意味着 Request 里存的是不合法的 JSON 字节——比如说，
		// 一段逐字捕获下来的供应商请求体。这时候要丢掉载荷、保留事件：
		// 少了一个请求体的 trace，依然是 trace，而 Seq 序号里的一个空洞，
		// 却是六个月后谁也解不开的谜。
		degraded := e
		degraded.Request = json.RawMessage(`{"trace_error":"request body was not valid JSON and was dropped"}`)
		line, err = marshalEvent(degraded)
		if err != nil {
			w.failLocked(fmt.Errorf("encode event %d (%s): %w", e.Seq, e.Kind, err))
			return
		}
	}
	line = append(line, '\n')

	// 持久性。字节直接落进文件：这条路径上没有 bufio.Writer，它的
	// 缺席正是整个设计所在。
	//
	// 一个 64KB 的缓冲会把几百个事件批处理进一个 syscall，然后在
	// Agent 被杀死的时候把它们全部丢失——而那一刻，恰恰就是 trace
	// 存在的意义所在。不做缓冲，每个事件只花一次 write(2)，写进
	// 内核页缓存不过几微秒，跟动辄几百毫秒的模型调用一比，这笔账
	// 根本算不上势均力敌。
	//
	// 我们故意在 f.Sync() 之前就收手。fsync 还能多扛过一次断电，
	// 但代价是要在 bus 锁内部，为**每一个文本 delta**都付出真实的
	// 磁盘延迟（SSD 上约 0.1ms，机械盘或网络挂载上约 10ms）：多付出
	// 三个数量级的代价，只为了防一种比我们已经应付过的故障（进程
	// 死掉）要罕见得多的故障模式（整台机器死掉）。这个 Write 一旦
	// 返回，数据就扛得住 SIGKILL、panic 和 os.Exit，不再需要我们
	// 帮忙。
	//
	// 每行一次 Write，也让这一行在 O_APPEND 下保持原子性——这就是
	// 不让并发写入者，把一条无法解析的记录拼接到文件中间的原因。
	if _, err := w.f.Write(line); err != nil {
		w.failLocked(fmt.Errorf("write to %s: %w", w.path, err))
	}
}

// 留意一下这里**没有**什么：没有 goroutine，没有 channel，没有
// 队列。
//
// "永不阻塞 bus"这句话，通常的答案是上一个异步写入器，而队列
// 一旦填满，恰好只有两种行为——阻塞生产者（我们正想避免的
// 事），或者丢弃事件（一份靠遗漏说谎的 trace，无声无息地，偏偏
// 就在你最想把它记录下来的那种负载下发生）。本地追加永远不会
// 无限等待，所以同步版本这两个问题都没有。bus 真正需要的规则是
// "不能无限等待"，不是"不能有 I/O"：没有 fsync，没有网络，没有
// 一把锁会在 channel 发送过程中被一直攥着。

func (w *TraceWriter) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failLocked(err)
}

func (w *TraceWriter) failLocked(err error) {
	w.dropped++
	if w.err != nil {
		return // 已经报告一次；保持安静并继续计数
	}
	w.err = err
	if w.warn != nil {
		w.warn("trace: %v — recording is disabled for the rest of this session", err)
	}
}

// Close 不会排空任何东西（因为什么都没缓冲），只负责报告损失
// 了多少。
//
// 它是幂等的，因为 main 会用 defer 调用它，而信号处理器也可能
// 调用它；如果第二次调用 Close 返回了错误，会让一次有序的关闭
// 看起来像一次失败。
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

// marshalEvent 编码一个事件时，HTML 转义是**关闭**的。
//
// json.Marshal 会把 <、> 和 & 转义成 \u003c、\u003e 和 \u0026，而——
// 这是咬人的部分——encoding/json 在**json.RawMessage 内部**也会
// 这样做，同时还会把它压缩。Event.Request 是一个 RawMessage，
// 装着适配器实际发出去的准确字节；两个适配器都特意用
// SetEscapeHTML(false) 编码，正是因为 shell Agent 的请求里大多是
// `2>&1`、`>/tmp/out` 和 `<<EOF` 这样的内容。
//
// 所以少了这一步，适配器小心做到的一切，都会在文件这一层被
// 悄悄推翻：
//
//	发布：{"command":"ls 2>&1 <in"}
//	跟踪：{"command":"ls 2\u003e\u00261 \u003cin"}
//
// 没有什么会报错，JSON 是等价的，每个解码它的消费者都能拿到
// 正确的字符串。破的是那句承诺：events.go 把 Request 称作"即将
// 发送的准确字节"，阶段 06 的线上视角承诺"字节对字节"，而一次
// 真实运行和它的重放之间的字节级比较，会显示出一个和这件事
// 完全对应的 diff。trace 是证据；它一旦不再是逐字节相同，就
// 不再是关于字节的证据。
//
// 这是在一份真实的 trace 里发现的：里面所有 24 个记录下来的
// 请求，都带着这些转义。
func marshalEvent(e Event) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	// Encoder.Encode 会追加一个 Marshal 不会加的换行；调用者自己
	// 又加了一个，两个换行叠在一起，就会在 JSONL 文件中间变成一个
	// 空行。
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
