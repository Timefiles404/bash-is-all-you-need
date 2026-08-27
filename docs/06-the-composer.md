# 阶段 06：composer

一份 trace 里装着两个故事，你用过的工具，无一例外只给你看头一个。

```
GOD     发生过什么。每一个事件，连同它的耗时、token、退出码和权限闸裁决
        ——包括那些从来没送到模型面前的东西。

MODEL   模型看见了什么。不是复原出来的：是真正的字节，从阶段 02 存在的
        第一天起就在记录的 request 事件里解出来。

WIRE    那些字节，原封不动，留给答案藏在标点里的时候。
```

三个视角都做出来，这才是重点。**Agent 的 bug 就住在前两者的落差里**，
而落差这东西，单靠一个视角是照不出来的。

```sh
go build -o agent ./stages/06-the-composer
./agent --composer session.jsonl        # 不用 key，不联网，不需要供应商
```

---

## 这一章就是为了那一行

随便挑一次压缩之后的调用，切到模型视角：

```
  call 12 of 24   openai · mimo-v2.5 · max_tokens 4096 · 16.4kB
  629 events happened so far · the model can see 11 messages · 0 cache marks · tools: bash
  ⚠ 1 compaction(s) happened before this call: everything below is what SURVIVED, not what happened
```

**629 个事件已经发生。模型能看见十一条消息。** 压缩之前，这两个数字
一起往上走；压缩之后，它们就此分家，而且再也合不回去。

所有"Agent 把我说过的话忘了"的 bug，都是这一行。"它老在重做已经
做过的事"是这一行，"它自己跟自己打架"也是这一行。它们本来就是
同一个 bug——*你问的那个东西，根本不在模型的上下文里*——而聊天记录
给不了你这个答案，因为聊天记录画的是上帝视角，然后管它叫对话。

两个视角还能照出另外四处差异，处处都正常，处处都只站一边就看不见：

- 模型推理了四百个 token，**下一个请求里一个字都没剩**，因为思考内容
  会从历史里被丢掉（阶段 03）。
- 用户敲了九个词，模型收到的是这九个词**外加一整块环境信息**，而它
  对这块信息只字不提（阶段 05）。
- 命令打印了 40kB，递到模型手上的是 8kB，**外带一个截断标记**
  （阶段 01）。
- `cache breakpoint` 标记落在两个特定的块上，压缩之后**那个滚动的标记
  早跑到别处去了**（阶段 04）。

---

## TUI 到底是什么东西

把框架剥掉，剩下的就是三个函数加一个 `select`：

```
bytes → key           解码终端送过来的字节                     keys.go
state + key → state   这串字节是什么意思                       tui.go
state → lines         画面该长成什么样                         views.go
```

```go
for {
    select {
    case chunk := <-in:      // 键盘
    case <-escTimer:         // Escape 的歧义，见下文
    case <-t.resize:         // 窗口尺寸变了
    }
    c.draw(t)
}
```

这个循环三十行。难的东西全在它周围那三个文件里：终端借了得还，键盘
说的是一门自带歧义的语言，还有列不等于字节。框架把这三样全藏起来了，
藏得挺好——一直好到其中一样出岔子，而你根本不知道是哪一样。

整套东西的依赖：标准库，外加 `golang.org/x/sys`——Unix 上拿它调三个
ioctl，Windows 上调五个控制台接口。和别的阶段一模一样。

---

## 进了原始模式，你就签下一份契约

TUI 得从终端手里拿走四样东西，而这四样，样样都是**对一份不归你所有的
资源做全局改动**：

| | 为什么 |
|---|---|
| 原始模式 | 按键变成字节送进来；Ctrl-C 不再是信号 |
| 备用屏 | 用户的滚屏先搁到一边，事后原样还回去 |
| 鼠标报告 | 点击和滚轮以转义序列的形式送进来 |
| 括号粘贴模式 | 粘进来的文本外面裹着标记，不会被当成按键执行 |

打开它们，四条 `printf` 就够了。难的是关掉：全世界只有打开它们的那个
进程知道该怎么关。要是它没关就先死了，用户就被撂在一个 shell 里——没有
回显，没有行编辑，没有光标，连鼠标选中都是坏的。懂行的人会敲 `reset`。
大多数人直接把窗口关掉。

所以这就是阶段 01 那一课，只是这回对准了另一份资源。出口一共四个，
真正的 TUI 得把四个都堵上：

```go
fn 正常返回          defer 会跑
fn 返回 error        defer 会跑，而且错误是在恢复**之后**才打印的——
                     打在用户真正的屏幕上，而不是一块马上就要被丢掉的
                     备用屏上
fn panic            defer 会跑，然后 panic 被重新抛出，所以栈回溯落在
                     一个显示得出来的终端上
SIGINT / SIGTERM     处理器先恢复，再把自己重置成默认行为，然后把信号
                     重新发给自己的进程
```

最后那条是刻意的，没写成 `os.Exit(130)`。被 SIGTERM 杀掉的进程
就该*如实报告*自己死于 SIGTERM——它的父进程可能是 shell，可能是
进程守护，也可能是一套分得清"死于信号"和"非零退出"的测试宿主。
收拾干净可以，但别在自己怎么死的这件事上撒谎。

还有一条规矩，它悄没声地废掉了一个在别处一向正确的习惯：

> **一旦进了原始模式，`os.Exit` 和 `log.Fatal` 就是 bug。**

它们会跳过 defer。埋在三层调用底下的那句 `log.Fatalf("bad config")`
——Go 里再平常不过的一行——如今的效果是：终端留在坏掉的状态，*而且*
把消息打在一块用户永远看不到的备用屏上。

---

## Escape 键是真的有歧义

输入缓冲区末尾孤零零躺着一个 `\x1b`，它要么就是 Escape 键，要么是某个
还没到齐的序列的头一个字节。**光看字节，没有哪个解码器分得出来。**

所以解码器干脆不猜：

```go
decodeKey(buf)        // 孤零零一个 ESC → ok=false："我还要更多字节"
decodeKeyFinal(buf)   // 超时过后仍然什么都没来，才调它 → keyEsc
```

等多久是策略，策略归事件循环管，因为它手里有时钟；不归解码器管，因为
解码器没有时钟，也不该有：

```go
if len(buf) > 0 {
    escTimer = time.After(50 * time.Millisecond)
} else {
    escTimer = nil            // nil channel 永远阻塞 = 等于没上膛
}
```

计时器的上膛和解除都在这一行，整套机制就这么多。

有两件事值得带走。**你用过的每一个终端程序里，Escape 都慢那么一点点，
根子就在这儿**——vim 也不例外，而且这在它们任何一个里头都不算 bug。
还有，**解码器之所以能测，恰恰因为它没有时钟**：一个自己决定等多久的
函数，想测就只能干等。

输入语言的其余部分也是这套章法。方向键来的时候可能是 `\x1b[A`，
*也*可能是 `\x1bOA`，取决于终端在不在应用光标模式——只认前一种的
解码器一路好用，直到有人把它放进 `tmux` 里跑。Home 和 End 有**八**种
不同的写法。括号粘贴要是被切成两半，必须报"不完整"，而不是把半截粘贴
交出去。鼠标坐标用 SGR 编码，因为老编码把列号塞进 `32 + n`，说不出
223 以后的列——这在宽终端上根本不是什么边界情况，那是屏幕的整个右半边。

---

## 列不是字节，也不是 rune

```go
len("你好世界")                    // 12   字节
utf8.RuneCountInString("你好世界")  //  4   个 rune
dispWidth("你好世界")               //  8   列       ← 终端只认这一个
```

`%-20s` 是按字节对齐的。拿它去排一列文件名，只要冒出头一个中文名字，
整列就全歪了。接着还有：

- 组合记号占 **0** 列——`"é"` 是 3 字节、2 个 rune、1 列
- 全角字符占 **2** 列——`"ＡＢ"` 是 4 列
- ANSI 转义占 **0** 列，所以 `dispWidth("\x1b[31mred\x1b[0m")` 是 3

跟着来的是三个后果，代码一个都躲不掉；任何一个搞砸，画出来的就是一帧
烂掉的画面：

**截断不能把宽字符劈开。** 只剩一列，下一个偏偏是占 2 列的 rune，
那就停在*它前面*，拿一个空格把剩下那列填掉。半个汉字不是渲染瑕疵，
那是一段终端根本没法解释的字节序列。

**截断也不能把转义序列劈开**——而且切口上要是还有个 SGR 开着，结果里
就得替它关上。否则这个颜色会渗进后面画的一切，一直渗到会话结束。

**多出一列的行会自动折行**，把它下面每一行都往下顶一格，整帧就毁了。
一处外观上的小失误，换来满屏塌方。所以 `frameBytes` 调的是 `truncCols`，
不是 `s[:w]`。

下面这条老实招了，因为装作没这毛病比毛病本身更糟：`width.go` 量 ZWJ
emoji 序列会量宽。`👨‍👩‍👧‍👦` 量出来是 8，画出来是 2。要真正修好得上字素簇
分割（UAX #29），那是一个实打实的依赖；而不修的话，症状是这样的：某个
用户，边框参差不齐，一周之后才反馈上来——不先在这儿交代一句，这症状
根本无从查起。

---

## 两个平台，还有一处改变了设计的不对称

和阶段 01 的 `proc_unix.go` / `proc_windows.go` 是同一个形状：契约
一模一样，机制完全两回事。

| | Unix | Windows |
|---|---|---|
| 设置 | 一个 `termios` 结构体 | 两个控制台模式位字段（输入、输出各一个） |
| 原始模式 | 清掉 `ICANON`、`ECHO`、`ISIG`、`OPOST`、… | 清掉 `ENABLE_LINE_INPUT`、`ENABLE_ECHO_INPUT`、`ENABLE_PROCESSED_INPUT` |
| ANSI | 默认就有 | **两个** handle 都得主动打开 |
| 大小 | `TIOCGWINSZ` | `GetConsoleScreenBufferInfo`，取的是 **window** 矩形，不是 buffer |
| 尺寸变化 | `SIGWINCH` | **没有任何东西会告诉你** |

**Windows 上没有 SIGWINCH**，所以 `watchResize` 拿 4Hz 轮询。这不是
图省事，这是选了 VT 这条路之后剩下的东西。Win32 的正路是从控制台输入
队列里读 `WINDOW_BUFFER_SIZE_EVENT` 记录——
可 `ENABLE_VIRTUAL_TERMINAL_INPUT` 干的恰恰就是把那个队列变成字节流，
你既然要了字节，就再也拿不到记录。这笔交易是：每 250ms 一次系统调用，
一直调下去，换来按键解码器只写一套，不用写两套。

两边的实现返回的是同一种 `<-chan struct{}`，容量 1，满了就丢，不阻塞，
所以事件循环分不出自己拿到的是哪一个。合并通知不是性能优化——拖动窗口
边框时，每挪过一个像素行都会发一次通知，而每一次说的都是同一句话：
"尺寸变了，自己去问"。

还有三件，毫无准备撞上去，一件能耗掉你一个下午：

- **`ENABLE_QUICK_EDIT_MODE` 默认开着**，鼠标会被拿去选文本，事件
  到不了你的程序。要清掉它，还得在同一次调用里把 `ENABLE_EXTENDED_FLAGS`
  一起设上——不这么做，控制台一声不响地无视你。"我的 TUI 在
  Windows 上收不到鼠标事件"，十有八九就是这个。
- **`ENABLE_VIRTUAL_TERMINAL_PROCESSING`** 要设在*输出* handle 上，
  转义序列才会被解释，而不是被原样打印出来。就一次 API 调用，却是
  "我的 Go TUI 在 Windows 上是坏的"这类报告里最常见的病根。
- **`TCGETS` vs `TIOCGETA`。** termios 是 POSIX 的，结构体可移植；
  读它写它的那些 ioctl 编号不可移植。Linux 和 BSD 挑了不同的名字、
  不同的值，没有一种写法能通吃两边，所以世上每一个终端库里都躺着一个
  带 build tag 的六行小文件。这个项目里有两个：`term_ioctl_linux.go`
  和 `term_ioctl_bsd.go`。

---

## 画得不闪

有两件事，`frameBytes` 是刻意不做的。

**它从不清屏。** 每帧之前来一发 `\x1b[2J`，这是闪烁的经典成因——因为
有那么一次刷新，终端上真的什么都没有。换成这样做：光标归位，重写一行
就顺手擦掉这一行（`\x1b[K`），于是每一格要么被覆写，要么被明确清空，
没有哪一帧是空的。

**它从不一行一行地写。** 一个缓冲区，一次 `Write`，外面套一层同步输出
标记（`\x1b[?2026h` … `\x1b[?2026l`），告诉现代终端：帧没画完之前
别动手。不认识这个序列的终端会直接忽略掉，所以无条件发它是安全的。

流式 delta 只在 `indexSession` 里折叠一次，上帝视角的每一处读到的，
都是折叠之后那个切片：

```
  389   32.40s reasoning_delta ×11  The user wants me to continue compacting the transcript…
  400   32.98s text_delta ×165      1. GOAL⏎ The user instructed the agent to read `wire-notes.md`…
```

一次流式响应就是一千个四字符的事件，一个事件占一行的话，这个视角
谁也滚不动。折叠只在一个地方做，因为同一个行号，对渲染器是一个意思、
对点击处理又是另一个意思，这种 bug 只有等谁动了鼠标才会露头。两个
数字都显示——帧数和字符数——因为它们的比例就是这条流的形状；
哪天供应商改成一个 token 发一个 delta，也只有在这里看得出来，
别处都看不出来。

---

## 这个 TUI 能拿去 grep

```sh
./agent --composer-dump session.jsonl --view model --call 12 --width 96
```

这不是什么调试暗门。凡是你想 diff、想 grep、想贴进 issue、想在 CI 里
断言的东西，TUI 都是死路一条——而*"第 12 次调用时模型看见了什么"*，
恰恰就是那种答案你想接进管道的问题：

```sh
# 一次压缩前后，模型视角里到底变了什么？
diff <(agent --composer-dump t.jsonl --view model --call 11) \
     <(agent --composer-dump t.jsonl --view model --call 12)
```

它一共只花了八行，因为渲染和绘制本来就是两个分开的函数——`views.go`
把会话变成 `[]string`，`term.go` 负责把 `[]string` 画出来。TUI 的测试
走的也是这条路：**输出只能靠按键按出来的 UI，就是没有测试的 UI。**

---

## 来自一次真实运行

压缩前后那一段的上帝视角，取自阶段 05 那个会话：

```
  379   31.28s usage            prompt 5258 (full 138 · write 0 · read 5120) · out 47
  380   31.33s response_end     tool_calls · 1946ms
  381   31.33s tool_call        $ sed -n '91,180p' wire-notes.md
  382   31.33s gate             allow
  383   31.33s command_start    sed -n '91,180p' wire-notes.md
  384   31.41s command_end      exit 0 · 82ms · 5.3kB TRUNCATED
  385   31.41s tool_result      5.3kB to model
  386   31.41s COMPACT_START    15 messages, ~7714 tokens — summarising messages 0–10, keeping 4
  387   31.41s request          openai · 1 messages · 0 cache marks · 11.6kB
  388   32.40s first_token      TTFT 991ms
  389   32.40s reasoning_delta ×11 The user wants me to continue compacting the transcript. Let me look at 
  400   32.98s text_delta ×165  1. GOAL⏎ The user instructed the agent to read `wire-notes.md` in eight 
  565   38.34s usage            prompt 3310 (full 3310 · write 0 · read 0) · out 506
  566   38.39s response_end     stop · 6975ms
  567   38.39s COMPACT_END      15 → 5 messages · ~7714 → ~3556 tokens · 6976ms
  568   38.39s cache_lost       the prompt prefix was rewritten — every cache entry from before this p
  569   38.39s turn_start       turn 2
  570   38.39s request          openai · 5 messages · 0 cache marks · 10.2kB
```

阶段 05 讲的那些，全在这一屏上。做摘要的那次调用是一次真调用
（`prompt 3310 · out 506`），而且全额付费，一分没省（`read 0`）。
压缩前那个请求带着 15 条消息、5,258 个 prompt token；压缩后那个只有
5 条消息、10.2kB。`command_end` 那行上的 `TRUNCATED` 说明，模型
拿到的比命令实际产出的要少。

在这些行里随便挑一行按 `m`，出来的就是那次请求装了哪些消息；按 `w`，
出来的就是字节。这个工具的全部本事就这些。

### 线上视角也没说实话

`WIRE` 承诺的是"那些字节，原封不动"。而把这个视角做出来的过程，恰好
证明这句承诺是假的——祸根是阶段 03 早就记下来的那个 bug，这回它待在
第三个谁也没去看过的地方。

`json.Marshal` 会把 `<`、`>` 和 `&` 转义掉，而 `encoding/json` 在压缩
JSON 的时候，**连 `json.RawMessage` 内部也照转不误**。`Event.Request`
就是一个 RawMessage，装的正是适配器发出去的原样内容；两个适配器都特意
用 `SetEscapeHTML(false)` 编码，就因为 shell Agent 的请求里满是
`2>&1`、`>/tmp/out` 和 `<<EOF`。可 trace 写入器随手用了普通的
`json.Marshal`，下一层就把这份功夫全撤销了：

```
posted:  {"command":"ls 2>&1 <in"}
traced:  {"command":"ls 2\u003e\u00261 \u003cin"}
```

没有任何东西报错。每一个去解码它的消费方，拿回来的字符串都是对的。
坏掉的是那句*声明*：`events.go` 管 `Request` 叫"马上要发出去的确切字节"，
可它在文件里走一趟往返之后就不是了。上面那个会话录下来的 24 个请求，
个个都带着转义。

修法是统一用一个编码器。教训是：**一道防御只要在某一层用了，就得在
每一层重新编码这些字节的地方都用上**——还有，在视角上写下"逐字节
原样"这句话，价值就在于总有一天会有人真的去核对。

### 说说它不是什么

composer 读的是 trace，它不是聊天窗口。这不是妥协，这是阶段 02 那个
决定——让 trace 当唯一的事实来源——现在开始兑现：

- 它**不要密钥、不要网络、不要供应商**，所以你能在一台从没配置过的
  机器上读会话
- 它能读**几周前**录下的会话，也能读此刻正在另一个终端里跑的会话——按
  `r` 重读文件，而 trace 是边跑边追加的，于是第二个终端就成了实时
  监视器，一点 IPC 都不用
- 它是**确定性的**，所以它才测得了

头一条在整整三个阶段里都是假话，而弄清它是怎么变假的，才是比这条本身
更值的一课。阶段 03 加进来一个供应商配置文件，把配置解析挪到了重放分
支的上面，连它自带的 `os.Exit(1)` 一起挪了上去。用来测试的每台机器上
环境变量都是配好的，解析次次成功，看上去一切正常。而在一台只有 trace
文件、别的什么都没有的机器上——也就是这个功能*本来就是给它做的*那台
机器上——`--replay` 打了一句"no provider configured"就退出了。

修法三行：把解析出来的错误带着走，别当场抛；只在真正需要活供应商的
那一处去检查它。**配置错误只该对依赖这份配置的代码致命，对别的一概
不致命**——而一个功能，卖点既然是"没有 X 也能用"，就得配一个
真不给它 X 的测试，否则这句话早晚烂成一行文档。

把它接到进程内一个活会话上只要一行——`bus.Subscribe(tui)`——道理跟
当初 JSONL 写入器和朴素渲染器各自只用一行是一样的。

---

## 练习

1. **打开一份阶段 04 的 trace**，在模型视角里一次一次调用往下翻，盯着
   `cache breakpoint` 标记挪位置。那就是滚动断点被画出来的样子。
2. **找一处分歧。** 随便挑一次调用，把它的上帝事件和模型消息都读一遍，
   列出只在一边有、另一边没有的东西。会比你以为的多。
3. **故意撕毁一次终端契约。** 往事件循环里塞一句 `log.Fatal`，跑一遍，
   看看之后你的 shell 变成什么样。然后再把它还原。
4. **把 Escape 超时设成 1ms**，在一条慢速 ssh 链路上按方向键。再把它
   设成 500ms，按一下 Escape。
5. **把 `frameBytes` 里的 `truncCols` 删掉**，换成 `s[:w]`。打开
   一份工作目录名带 CJK 字符的 trace，看着多出来的那一列把整帧毁掉。
6. **加一个 diff 视角。** 给两个调用序号，列出这两次之间变过的消息。
   要用的东西 `wireView` 里全都有了；有意思的地方在于想清楚：整个前缀
   都被重写过之后，"变了"到底算什么意思。
7. **实时订阅它。** 对 composer 调 `bus.Subscribe`，然后在它里面
   把 Agent 跑起来。真正的活儿不在接线，而在想清楚：用户正翻在
   别处的时候来了新事件，UI 该怎么办。

→ 下一站：[阶段 07：乘法](07-multiply.md)

→ 参考：[阶段 02：看见一切](02-see-everything.md)
和 [阶段 05：长生](05-live-forever.md)
