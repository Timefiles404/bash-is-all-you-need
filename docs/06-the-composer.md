# 阶段 06 — The Composer

一个 trace 讲述两个不同的故事，你用过的每个工具都只给你看第一个。

```
GOD     what happened.  Every event, with its timings, tokens, exit codes and
        gate verdicts — including the things that were never sent to the model.

MODEL   what the model saw.  Not a reconstruction: the actual bytes, decoded out
        of the request event stage 02 has been recording since it existed.

WIRE    those bytes, unmodified, for when the answer is in the punctuation.
```

构建这三个视角才是重点。**前两者之间的间隙，就是 Agent bug 藏身的地方**，单一视角看不出这道间隙。

```sh
go build -o agent ./stages/06-the-composer
./agent --composer session.jsonl        # no key, no network, no provider
```

---

## 这章存在的那一行

在任意一次压缩之后的调用上，打开模型视角：

```
  call 12 of 24   openai · mimo-v2.5 · max_tokens 4096 · 16.4kB
  629 events happened so far · the model can see 11 messages · 0 cache marks · tools: bash
  ⚠ 1 compaction(s) happened before this call: everything below is what SURVIVED, not what happened
```

**629 个事件已经发生。模型能看到十一条消息。** 压缩之前，这两个数字一起上升；压缩之后，它们永久分家，间隙再也不会闭合。

每一个"Agent 忘了我告诉过它的事"这种 bug，都是这一行；每一个"它老在重复已经做过的工作"，每一个"它自相矛盾"，也都是同一回事。它们其实是同一个 bug——*你问的东西根本不在模型的上下文里*——而聊天记录展示不出这一点，因为聊天记录渲染的是上帝视角，却把它叫作对话。

这两种视角还暴露了四个更多的差异，全都是常见的，全都从一边看不到：

- 模型推理用了四百个 token，**其中没有一个进了下一个请求**，因为思考的内容会被从历史里删掉（阶段 03）。
- 用户敲了九个词，模型收到的却是九个词**外加一个环境块**——这个环境块，模型从不提及（阶段 05）。
- 一个命令打印了 40kB，模型却只拿到 8kB，**外加一个截断标记**（阶段 01）。
- `cache breakpoint` 标记停在两个具体的块上，压缩之后**滚动的那个完全换了地方**（阶段 04）。

---

## TUI 实际上是什么

剥去框架，它就是三个函数和一个 `select`：

```
bytes → key           decoding what the terminal sent          keys.go
state + key → state   what that means                          tui.go
state → lines         what it should look like                 views.go
```

```go
for {
    select {
    case chunk := <-in:      // keyboard
    case <-escTimer:         // Escape 的歧义，下面
    case <-t.resize:         // 窗口改变了
    }
    c.draw(t)
}
```

那个循环只有三十行。真正困难的东西，都在它周围那三个文件里——终端用完要还回去，键盘说的是一种带歧义的语言，一个列也不等于一个字节。框架把这三样都藏了起来，这样很好——直到其中一个出问题，你却完全不知道是哪个。

整个项目的依赖：标准库，加上 `golang.org/x/sys`——Unix 上用三个 ioctl，Windows 上用五个控制台调用。和其他每个阶段一样。

---

## 原始模式下你接纳的契约

一个 TUI 需要向终端要来四样东西，而每一样，都是**对一份不属于你的资源做的全局性变异**：

| | 为什么 |
|---|---|
| 原始模式 | 按键以字节形式到达；Ctrl-C 不再是一个信号 |
| 备用屏 | 用户的滚屏历史会被妥善搁置一旁，事后原样归还 |
| 鼠标报告 | 点击和滚轮都会以转义序列的形式到达 |
| 括号粘贴模式 | 粘贴的文本到达时外面裹着一层标记，因此不会被当作按键执行 |

打开它们是四个 `printf`。关闭它们才是整个问题所在：打开它们的那个进程，是这世上唯一知道该怎么关掉它们的东西。如果它没来得及关掉就先死了，用户就会被撂在一个 shell 里——没有回显、没有行编辑、没有光标，连鼠标选择都是坏的。知道这招的人会输入 `reset`。大多数人直接关闭窗口。

所以，这其实就是阶段 01 的教训，只是这次换了一个不同的资源。退出路径一共四条，一个真正的 TUI 得把这四条都处理好：

```go
fn returns          the defer runs
fn returns an error the defer runs, and the error prints AFTER the restore —
                    on the user's real screen, not on an alternate screen that
                    is about to be discarded
fn panics           the defer runs, then the panic is re-raised, so the stack
                    trace lands on a terminal that can display it
SIGINT / SIGTERM    the handler restores, resets itself to the default, and
                    re-sends the signal to its own process
```

那最后一个是刻意的，而不是 `os.Exit(130)`。一个被 SIGTERM 杀死的进程应该*报告*它被 SIGTERM 杀死——它的父进程可能是一个 shell、一个监督者，或者一个会区分"信号致死"和"非零退出"的测试宿主。清理可以，但别在自己是怎么死的这件事上撒谎。

还有一个规则，它静静地使一个在其他地方是正确的习惯无效：

> **一旦进入了原始模式，`os.Exit` 和 `log.Fatal` 就是 bug。**

它们会跳过延迟函数。藏在三层调用之下的一句 `log.Fatalf("bad config")`——Go 里最平常不过的一行——现在会让终端坏掉，*而且*把消息打印在用户永远看不到的备用屏上。

---

## Escape 键真正存在歧义

输入缓冲区末尾孤零零的一个 `\x1b`，要么是 Escape 键本身，要么是某个还在陆续抵达的序列的第一个字节。**没有任何解码器能单从字节上分辨是哪一种。**

所以解码器干脆不去下判断：

```go
decodeKey(buf)        // lone ESC → ok=false: "I need more bytes"
decodeKeyFinal(buf)   // called after a timeout produced nothing → keyEsc
```

该等多久——这个策略属于事件循环，因为它有时钟；不属于解码器，因为解码器没有时钟，也不该有：

```go
if len(buf) > 0 {
    escTimer = time.After(50 * time.Millisecond)
} else {
    escTimer = nil            // a nil channel blocks forever = disarmed
}
```

那一行启用和禁用计时器，它就是整个机制。

两点值得记住。**这就是为什么在你用过的每一个终端应用里，Escape 键都感觉慢半拍**——vim 也不例外，而这在其中任何一个里都不是 bug。而且**解码器正好因为它没有时钟，才是可测试的**：一个自己决定超时的函数，只能靠等待来测试。

同样一套严谨的做法，贯穿了输入语言的其余部分。箭头键到达时是 `\x1b[A` *或* `\x1bOA`，取决于终端是否处于应用光标模式——只认得前一种写法的解码器，能撑到有人在 `tmux` 里运行它为止。Home 和 End 键会以**八**种不同的形式到达。一次被切成两半的括号粘贴必须报告"不完整"，而不能只交付一半的粘贴内容。鼠标坐标用 SGR 编码，是因为旧式编码把列号塞进 `32 + n`，表示不了 223 之后的列——这在宽终端上根本不是什么边界情况，而是屏幕的整个右半边。

---

## 一列不是一个字节，它也不是一个 rune

```go
len("你好世界")                    // 12   bytes
utf8.RuneCountInString("你好世界")  //  4   runes
dispWidth("你好世界")               //  8   columns   ← the only one a terminal cares about
```

`%-20s` 是按字节对齐的。把它用在一张文件名表格上，只要出现一个中文文件名，就会把整列拉得歪七扭八。接下来：

- 组合标记是**0**列——`"é"` 是 3 字节、2 rune、1 列
- 全角形式是**2**——`"ＡＢ"` 是 4 列
- ANSI 转义是**0**，所以 `dispWidth("\x1b[31mred\x1b[0m")` 是 3

代码必须处理三个后果，每一个只要搞错，出来的都是一帧破损的画面：

**截断不能切开宽字符。** 如果只剩一列空间，而接下来是个占 2 列的 rune，就要停在*它前面*，用一个空格把这个孤立的列补上。半个 CJK 字形不是什么渲染瑕疵，而是一段终端根本无法解析的字节序列。

**截断也不能切开转义序列**——要是切口处正好有个 SGR 还开着，就必须在结果里把它关上。不然颜色就会渗进它之后画的一切里，而且会渗到这个会话结束。

**溢出一列的行会自动换行**，把它下面的每一行都往下推一行，毁掉整个画面。一个外观上的小错误，一整个屏幕的崩坏。这就是为什么 `frameBytes` 调用的是 `truncCols`，而不是 `s[:w]`。

老实交代出来，因为假装没有这个限制，比这个限制本身更糟：`width.go` 测量 ZWJ emoji 序列时会量得太宽。`👨‍👩‍👧‍👦` 测量出 8，实际画出来却只占 2。
要正确修复，需要字素群集分割（UAX #29），这是一个真正的依赖；没有它的话，症状——一个用户反馈，边框参差不齐，整整一周后才有人发现——会让人完全摸不着头脑。

---

## 两个平台，和一个改变设计的不对称

这和阶段 01 的 `proc_unix.go` / `proc_windows.go` 是同一种形状：契约相同，机制完全不同。

| | Unix | Windows |
|---|---|---|
| 设置 | 一个 `termios` 结构 | 两个控制台模式位字段（输入和输出） |
| 原始模式 | 清除 `ICANON`、`ECHO`、`ISIG`、`OPOST`、… | 清除 `ENABLE_LINE_INPUT`、`ENABLE_ECHO_INPUT`、`ENABLE_PROCESSED_INPUT` |
| ANSI | 假定 | **两个** handle 都选择加入 |
| 大小 | `TIOCGWINSZ` | `GetConsoleScreenBufferInfo`，**window** rect 不是 buffer |
| 调整大小 | `SIGWINCH` | **什么都不告诉你** |

**Windows 上没有 SIGWINCH**，所以 `watchResize` 以 4Hz 轮询。这不是图省事的办法；这是选定了 VT 路径之后剩下的唯一选择。Win32 的方式是从控制台输入队列里读出 `WINDOW_BUFFER_SIZE_EVENT` 记录——但 `ENABLE_VIRTUAL_TERMINAL_INPUT` 恰恰会把那个队列变成一串字节流，一旦你要了字节，就再也拿不到记录了。代价是：每 250 毫秒一次、永远不停的系统调用，换来的是只用一个按键解码器，而不是两个。

两个实现都返回同一种 `<-chan struct{}`，容量为 1，只丢弃不阻塞，所以事件循环没法分辨自己收到的是哪一个。合并处理不是什么性能优化——拖动窗口边缘时，每一个像素行都会触发一次通知，而每一次的意思都一样："大小变了，去问一声"。

还有三件事，你要是毫无准备地碰上，每一件都得花掉一个下午：

- **`ENABLE_QUICK_EDIT_MODE` 默认是打开的**，会让鼠标去选取文本，而不是把事件送到你的程序里。要清除它，还得在同一次调用里设置 `ENABLE_EXTENDED_FLAGS`——不这么做，控制台会不声不响地无视你。"我的 TUI 在 Windows 上收不到鼠标事件"，通常就是这个原因。
- **`ENABLE_VIRTUAL_TERMINAL_PROCESSING`** 设在*输出* handle 上，转义序列才会被解释，而不是被原样打印出来。只是一个 API 调用，却是"我的 Go TUI 在 Windows 上坏掉了"这条报告最常见的病因。
- **`TCGETS` vs `TIOCGETA`。** termios 属于 POSIX 标准，这个结构体本身是可移植的；但读、写它的那些 ioctl 编号却不是。Linux 和 BSD 用的名字不同，值也不同，没有一种写法能通吃两边——这就是为什么世界上每一个终端库，都有一个带着构建标签的六行小文件。这个项目就有两个：`term_ioctl_linux.go` 和 `term_ioctl_bsd.go`。

---

## 无闪烁地绘制

两件 `frameBytes` 刻意不做的事。

**它从不清除屏幕。** 每帧之前来一次 `\x1b[2J` 是闪烁的经典成因——因为清屏之后、新一帧画出来之前的那一次刷新里，终端上真的什么都没有。于是换成这样：把光标归位，重写每一行时顺手擦掉这一行（`\x1b[K`），这样一来，每个字符格要么被直接覆写，要么被显式清空，没有一帧会是空白的。

**它从不逐行写。** 一个缓冲，一个 `Write`，用同步输出标记包裹（`\x1b[?2026h` … `\x1b[?2026l`），告诉现代终端：帧没画完之前不要动手绘制。不认得这个序列的终端会直接忽略它，所以无条件发送它是安全的。

流式 delta 会在 `indexSession` 里统一折叠一次，上帝视角的每个部分，读到的都是折叠后的切片：

```
  389   32.40s reasoning_delta ×11  The user wants me to continue compacting the transcript…
  400   32.98s text_delta ×165      1. GOAL⏎ The user instructed the agent to read `wire-notes.md`…
```

一次流式响应就是一千个只有四个字符的事件；要是每个事件占一行，这个视角就没人滚得动了。折叠处理只放在一个地方，是因为同一个行索引，要是对渲染器是一个意思、对点击处理器又是另一个意思，那就成了一个只有用户动了鼠标才会暴露的 bug。两个数字都会显示——帧数和字符数——因为它们的比例就是这条流的形状；一旦有供应商改成每个 token 发一次 delta，也只有在这里才能看出来，别的地方都看不出。

---

## 一个你能 grep 的 TUI

```sh
./agent --composer-dump session.jsonl --view model --call 12 --width 96
```

这不是一个调试舱口。一个 TUI，对任何你想拿去 diff、grep、粘贴进 issue 里，或者在 CI 里做断言的东西来说，都是一条死路——而*"模型在第 12 次调用时看到了什么"*，正好就是那种你想用管道解决的问题：

```sh
# what changed in the model's view across a compaction?
diff <(agent --composer-dump t.jsonl --view model --call 11) \
     <(agent --composer-dump t.jsonl --view model --call 12)
```

只花了八行，因为渲染和绘制本来就是两个分开的函数——`views.go` 把一个会话变成 `[]string`，`term.go` 负责把 `[]string` 画出来。TUI 也正是这样被测试的：**一个只能靠按键才能产生输出的 UI，就是一个没有测试的 UI。**

---

## 来自一次真实运行

一次压缩前后的上帝视角，来自阶段 05 那个会话：

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

阶段 05 主张的一切，都在这个屏幕上。负责生成摘要的那次调用是一次真实的调用（`prompt 3310 · out 506`），而且完全按全价计费（`read 0`）。压缩前的请求携带 15 条消息、5,258 个 prompt token；压缩后的请求只有 5 条消息、10.2kB。`command_end` 那一行上的 `TRUNCATED` 说明，模型拿到的比这条命令实际产生的要少。

在这些行里随便挑一行按下 `m`，就能看到那次请求包含的消息；按 `w` 则能看到字节内容。这就是这个工具的全部。

### Wire 视角也没有说真话

`WIRE` 承诺的是"那些字节，未经修改"。而构建这个视角的过程，恰恰证明了这个承诺是假的——原因是一个阶段 03 早就记录过的 bug，这次它藏在了第三个、此前没人看过的地方。

`json.Marshal` 会转义 `<`、`>` 和 `&`，而 `encoding/json` 在压缩时，**在 `json.RawMessage` 内部也一样会转义**。`Event.Request` 是一个 RawMessage，装的正是适配器发出去的东西，而两个适配器都特意用 `SetEscapeHTML(false)` 来编码，正是因为 shell Agent 的请求里大多是 `2>&1`、`>/tmp/out`、`<<EOF` 这样的内容。trace 写入器随后却用了朴素的 `json.Marshal`，在下一层就把这一切都撤销了：

```
posted:  {"command":"ls 2>&1 <in"}
traced:  {"command":"ls 2\u003e\u00261 \u003cin"}
```

没有任何东西会报错。每一个解码它的消费者，拿回来的字符串都是对的。真正出问题的，是这个*声明*本身：`events.go` 把 `Request` 称作"马上要发送出去的确切字节"，可是在文件里走一趟往返之后，事实并非如此。前面那个会话里，录到的全部 24 个请求都带着转义。

修复是一个编码器。教训是：**防御措施只要用在一层，就必须用在每一个会重新编码这段字节的层上**——而**在视角上写"byte for byte"这句话的价值，就在于终究会有人去核实它**。

### 它不是什么

composer 读一个 trace；它不是一个聊天窗口。那不是一种妥协，而是阶段 02 那个决定换来的回报——那个决定就是：让 trace 成为真理的来源：

- 它**不需要密钥、不需要网络，也不需要供应商**，所以你能在一台从未配置过的机器上读一个会话
- 它既能用在**几周前**录好的会话上，也能用在此刻正在另一个终端里运行的会话上——`r` 会重新读取文件，而 trace 是实时追加写入的，所以第二个终端就是一个完全不需要 IPC 的实时监视器
- 它是**确定性的**，这就是为什么它能被测试

那第一条项目符号，在长达三个阶段的时间里都是个谎言，而搞懂它是怎么回事，才是比这条项目符号本身更好的一课。阶段 03 引入了一个供应商配置文件，把配置解析这一步放在了重放分支判断之前——同时也把它自带的 `os.Exit(1)` 一起带了进来。每一台用来测试的机器上，环境变量都是配置好的，所以配置解析每次都成功，看起来一切正常。而在一台只有 trace 文件、别的什么都没有的机器上——也就是这个功能*本来是为*之设计的那台机器——`--replay` 打印出"no provider configured"，然后就退出了。

修复只用了三行：携带分辨率错误，而不是直接抛出它，只在真正需要一个可用供应商的那个地方，才去检查这个错误。**一个配置错误，应该只对依赖这个配置的代码是致命的，对其他任何代码都不该是**——一个卖点是"没有 X 也能正常工作"的功能，就需要一个真的不给它 X 的测试，不然这个声明就只会退化成一份文档。

把它接到一个进程内的活会话上，只需要一行——`bus.Subscribe(tui)`——原因跟 JSONL 写入器、朴素渲染器当初都只用一行实现是一样的。

---

## 练习

1. **打开一份阶段 04 的 trace**，在模型视角里逐个调用地往下翻，看 `cache breakpoint` 标记移动。那正是滚动断点被画出来的样子。
2. **找一个分歧。** 挑一次调用，读它的上帝事件和它的模型消息，列出在一边出现、另一边却没有的一切。结果会比你预期的多得多。
3. **故意破坏一次终端契约。** 把一句 `log.Fatal` 放进事件循环里，运行它，看看运行之后你的 shell 变成什么样。然后把它放回去。
4. **把 Escape 超时设成 1ms**，在一条慢速 ssh 链路上试试箭头键。再把它设成 500ms，按一下 Escape。
5. **从 `frameBytes` 里删掉 `truncCols`**，换成 `s[:w]`。打开一份工作目录名字带 CJK 字符的 trace，看着一列溢出把整个画面毁掉。
6. **加一个 diff 视角。** 两个调用索引，加上它们之间发生变化的消息。你需要的一切都已经在 `wireView` 里了；有意思的地方在于，要弄清楚当整个前缀都被重写之后，"改变"到底意味着什么。
7. **实时订阅它。** 对 composer 调用 `bus.Subscribe`，在它里面运行 Agent。真正的工作不是管道，而是要想清楚：当用户正翻到别处看东西时又来了新事件，UI 该怎么处理。

→ 下一步：阶段 07 — 乘法 *(计划中)*

→ 参考：[阶段 02 — 看清一切](02-see-everything.md)、[阶段 05 — 永远活着](05-live-forever.md)
