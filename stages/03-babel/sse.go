// Stage 03——SSE framing，仅此而已。
//
// 这个文件从 stage 02 的 sse.go 中切出来，切割就是这一章
// 的重点。这里的一切都是关于*transport*：线上的字节如何
// 变成离散的帧。这里没有任何东西知道 token 是什么、工具
// 调用长什么样，或者另一端是哪个供应商。
//
// 那个分离，就是让同一个读取器能服务两个几乎在所有其他
// 事情上都不一致的协议：OpenAI 表面只发送带 `[DONE]`
// 哨兵的 `data:` 行，Anthropic 表面发送 `event:` +
// `data:`，完全没有哨兵。同样的 framing，不同的有效负载。
// 想看懂这些负载的那一半，去看 openai.go 和 anthropic.go。
package main

import (
	"bufio"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// 第一半：**SSE** 框架。有意做到与协议无关。
// ---------------------------------------------------------------------------

// sseFrame 是一个解码后的 SSE 帧。对于省略了 event: 行的流，
// Name 会是 ""——这就是这个阶段所能看到的每一个帧，因为
// 这个端点的 OpenAI 一侧，只发送 `data:`（§B4：在整个流中，
// `grep -c '^event:'` = 0）。Name 之所以还是存在，是因为阶段
// 03 里的 Anthropic 一侧，确实会用到 `event:` 行，而一个要
// 等到以后才被教会认识它们的读取器，在这之前的这段时间
// 里，本身就是错的。
type sseFrame struct {
	Name string
	Data string
}

// readSSE 会对每一帧都调用 fn，直到流结束为止。它必须
// 处理：只有 `data:` 行的帧、带 `event:` + `data:` 的帧、多行
// 数据、空行分隔、CRLF，以及以 ':' 开头的注释行。如果 fn
// 返回一个非 nil 的错误，就会停止扫描，并把那个错误返回。
//
// 注意它**不**做的事：它完全不知道 `[DONE]` 是什么意思。
// 一个哨兵，是载荷协议的属性，不是框架的属性——把这个
// 知识硬塞进这一层里，正是你最终会没法复用这个读取器的
// 原因。
//
// 实现里有三个细节，每一个都值得算作一个 bug：
//
//  1. bufio.Reader，不是 bufio.Scanner。Scanner 默认会拒绝
//     超过 64KB 的 token，并在最糟糕的时刻，把这一点报告
//     为错误——一个在单个 delta 里被原样回显回来的大型
//     工具结果，正是会触发这个问题的那种帧，而这种情况，
//     只会在生产环境里才发生。
//
//  2. 流的最后一行，会在 EOF 被处理**之前**，先被处理掉。
//     ReadString 会把它设法读到的字节，连同 io.EOF 一起
//     交回来，所以一个没有以空行收尾就关闭连接的服务器，
//     它的最后一帧，仍然会好好地待在 `line` 变量里。如果
//     你先检查错误，就会无声无息地丢掉每一个这种流的
//     最后一帧——而这一帧，通常正是携带着 usage 的那一个。
//
//  3. 行尾会被逐个剥离（先 `\n`，再 `\r`），而不是用一个
//     cutset 一起处理，所以那些确实是以一个回车符结尾的
//     合法数据，会把它保留下来。一个单独的 CR 终止符——
//     SSE spec 允许、没有人会发出、§B4 里也没有出现——
//     超出范围；在这里，也是观察赢过 spec——就像这份文件
//     里其他所有地方一样。
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // 每个 `data:` 行一项；分发时用 "\n" 连接
		sawData bool     // 是否有**任何**数据行到达，不是是否非空
	)

	// 分发会交付目前为止构建好的帧，并重置缓冲区。
	//
	// 规范说没有数据行的帧不是事件，这里就是这个规则：它让连续的
	// 空白行和裸 keep-alive 注释不产生代价，而不是引发一阵空帧。
	// 有一个数据行恰好为空的帧**确实**会分发，这是有意越过规范一步
	// ——这是调试工具，可见的空帧比无声丢弃的帧教得更多。
	dispatch := func() error {
		if !sawData {
			name = ""
			return nil
		}
		f := sseFrame{Name: name, Data: strings.Join(data, "\n")}
		name, data, sawData = "", data[:0], false
		return fn(f)
	}

	for {
		line, err := br.ReadString('\n')

		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")

			switch {
			case line == "":
				// 空行：帧结束。
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// 注释。代理和网关把这个当 keep-alive 发送，这样空闲连接就不会
				// 在生成过程中被回收。它们什么都不带，也不能终止当前帧——注意
				// 这种情况得在下面的字段拆分之前测试，否则 `: ping` 会解析成一个
				// 名字为空的字段。

			default:
				// `field: value`，其中只有**第一个**冒号分隔，值的单个前导空格被
				// 剥离。两个都很重要：这里的每个载荷都是 JSON，所以值里全是冒号，
				// 空格规则错误会把每个消息的每个字节移位一位。
				field, value := line, ""
				if i := strings.IndexByte(line, ':'); i >= 0 {
					field, value = line[:i], line[i+1:]
					value = strings.TrimPrefix(value, " ")
				}
				switch field {
				case "event":
					name = value
				case "data":
					data = append(data, value)
					sawData = true
				}
				// `id:` 和 `retry:` 是规范字段，用于重新连接到断开的流。两个都
				// 没有出现在 §B4，这个端点不提供恢复半生成完成的功能，所以它们
				// 被忽视而不是半支持。
			}
		}

		if err != nil {
			if err == io.EOF {
				// 流结束了。任何还在缓冲的东西是一个真实的帧，只是没有得到它
				// 的终止空行——Anthropic 一侧（§B6）正是这样结束的，关闭连接
				// 时根本没有哨兵。
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 下半部分：OpenAI 块模式。
// ---------------------------------------------------------------------------

// sseDoneSentinel 是 OpenAI 协议用来说"就这么多"的帧。
//
// **决策：我们跳过它并继续排空到 EOF**。它不是这里的停止信号。
//
// §B4 帧 13 是一个真实的帧，在哨兵**之后**到达：
// `{"choices":[],"cost":"0"}`。每个规范兼容的客户端在 `[DONE]` 处停止
// 读取并丢弃它。有三个理由不这样做：
//
//   - 正确性。成本帧是这个端点试图给我们的数据。
//   - 连接卫生。放弃还有字节在其中的响应体意味着 HTTP 传输不能
//     把连接返回到 keep-alive 池；你每回合支付一次新 TLS 握手，
//     永远不会注意到为什么。
//   - 健壮性。如果使用量曾经在哨兵之后移动——在一个已经在那里放
//     `cost` 的端点，那不是疯狂的假设——一个停止得早的客户端报告
//     零 token 且充满信心地错了。
//
// 排空什么都不花：服务器之后立即关闭流。
const sseDoneSentinel = "[DONE]"
