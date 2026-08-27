// 阶段 03——SSE 分帧，别的什么都不干。
//
// 这个文件是从阶段 02 的 sse.go 里切出来的，而这一刀就是本章的要
// 点。这里的一切都关于**传输**：线上的字节怎么变成一帧一帧离散的东
// 西。这里没有任何代码知道 token 是什么、工具调用长什么样，或者对面
// 是哪家厂商。
//
// 正是这道分隔，让同一个 reader 能服务两个协议，而这两个协议在几乎
// 所有别的事情上都谈不拢：OpenAI 那一侧只发 `data:` 行，配一个
// `[DONE]` 哨兵；Anthropic 那一侧发 `event:` 加 `data:`，一个哨兵都
// 没有。分帧一样，payload 不一样。知道这些事的那两半，见 openai.go
// 和 anthropic.go。
package main

import (
	"bufio"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// 前一半：SSE 分帧。故意做成与协议无关。
// ---------------------------------------------------------------------------

// sseFrame 是解出来的一帧 SSE。流里不带 event: 行时 Name 是 ""——本阶段会
// 见到的每一帧都是这样，因为这个端点的 OpenAI 那边只发 `data:`（§B4：整
// 条流上 `grep -c '^event:'` = 0）。Name 还是留着，因为阶段 03 的
// Anthropic 那边确实用 `event:` 行；等到那时再来教读取器认它，中间这段时
// 间它就是错的。
type sseFrame struct {
	Name string
	Data string
}

// readSSE 对每一帧调用 fn，直到流结束。它必须应付：只有 `data:` 行的帧、
// `event:` + `data:` 的帧、多行 data、空行分隔、CRLF，以及以 ':' 开头的注
// 释行。fn 返回非 nil 的 error 会中止扫描，并把那个 error 原样返回。
//
// 注意它*不*做什么：它根本不知道 `[DONE]` 是什么意思。哨兵是载荷协议的属
// 性，不是分帧的属性，把这份知识往下压到这里，最后就是读取器复用不了。
//
// 实现里有三个细节，每一个都对得起一个 bug：
//
//  1. 用 bufio.Reader，不用 bufio.Scanner。Scanner 默认拒收超过 64KB 的
//     token，而且是在最糟的时刻把这事报成错误——某个大工具结果在一条
//     delta 里回显出来，正是踩中它的那一帧，而且这种事只会在生产环境发
//     生。
//
//  2. 流的最后一行是在处理 EOF *之前*就先处理掉的。ReadString 会把已经
//     读到的字节连同 io.EOF 一起交回来，所以服务端不带结尾空行就关闭
//     时，最后一帧还躺在 `line` 里。先判错误，你就会悄悄丢掉每一条这种
//     流的最后一帧——而那通常就是带 usage 的那帧。
//
//  3. 行尾是一次剥一个（先 `\n`，再 `\r`），不是拿 cutset 一把切掉，所以
//     正当地以回车结尾的数据能把回车留住。单独一个 CR 作终止符——SSE 规
//     范允许，没人真发，§B4 里也没有——不在处理范围内；这里跟这个文件其
//     他地方一样，观测赢过规范。
func readSSE(r io.Reader, fn func(sseFrame) error) error {
	br := bufio.NewReader(r)

	var (
		name    string
		data    []string // 每个 `data:` 行一项；dispatch 时用 "\n" 拼起来
		sawData bool     // 有没有来过*任何* data 行，而不是它是否非空
	)

	// dispatch 把攒到此刻的这一帧交出去，然后清空缓冲。
	//
	// 规范说没有 data 行的帧不算事件，这里就照这条办：一连串空行、光秃秃的
	// keep-alive 注释于是一点代价都不花，而不是抖出一串空帧。带一条恰好为空
	// 的 data 行的帧*是*会 dispatch 的，这是有意越过规范一步——这是个调试工
	// 具，看得见的空帧比被悄悄丢掉的空帧教得更多。
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
				// 空行：一帧到此结束。
				if derr := dispatch(); derr != nil {
					return derr
				}

			case strings.HasPrefix(line, ":"):
				// 注释行。代理和网关拿它当 keep-alive 发，免得空闲连接在
				// 生成到一半时被回收。它们什么都不带，也不能结束当前这一
				// 帧——另外注意，这个 case 必须放在下面的字段切分之前判，
				// 否则 `: ping` 会被解析成一个名字为空的字段。

			default:
				// `field: value`，只有**第一个**冒号算分隔，值开头恰好剥掉
				// 一个空格。两条都要紧：这里每份载荷都是 JSON，值里全是冒
				// 号；空格规则搞错，每条消息的每个字节都会错开一位。
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
				// `id:` 和 `retry:` 是规范里用来重连断流的字段。§B4 里这两
				// 个都没出现，而这个端点也不提供"续上生成到一半的补全"这
				// 回事，所以直接忽略，而不是支持一半。
			}
		}

		if err != nil {
			if err == io.EOF {
				// 流结束了。缓冲里还剩下的是一帧真帧，只是没等到它那行终
				// 止空行——Anthropic 那边（§B6）正是这么收尾的：连接一关，
				// 连哨兵都没有。
				return dispatch()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 后一半：OpenAI 的 chunk 结构。
// ---------------------------------------------------------------------------

// sseDoneSentinel 是 OpenAI 协议用来说"就这些了"的那一帧。
//
// **决定**：跳过它，**继续读到 EOF**。它在这里不是停止信号。
//
// §B4 的第 13 帧是一帧真帧，而且到得比哨兵*还晚*：
// `{"choices":[],"cost":"0"}`。每个守规范的客户端读到 `[DONE]` 就停，把它
// 扔了。不当这种客户端，有三个理由：
//
//   - 正确性。cost 帧是这个端点想给我们的数据。
//   - 连接卫生。body 里还有字节就把响应丢开，HTTP transport 就没法把连接
//     还回 keep-alive 池；于是你每个回合都付一次全新的 TLS 握手，而且一直
//     不知道为什么。
//   - 健壮性。万一哪天 usage 挪到哨兵后面——在一个已经把 `cost` 放在那儿
//     的端点上，这算不上什么离谱的假设——提前收手的客户端会报出零 token，
//     而且报得理直气壮。
//
// 读到底不花什么代价：服务端紧接着就把流关了。
const sseDoneSentinel = "[DONE]"
