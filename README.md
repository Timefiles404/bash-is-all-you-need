# bash is all you need

**一个只有一个工具、配一套玻璃座舱的 coding agent。**

许多教程会教你怎么写一个 Agent 主循环。在 2026 年，那是一个下午的工作，这个仓库在第 00 阶段用一个文件把它做完，没有任何依赖。然后它用剩下的阶段讲没人教的那部分：

> **看清楚每一个 token、每一毫秒、每一分钱实际去了哪里。**

从"我写了个 Agent 主循环"到"我能在生产环境跑一个 Agent"之间的差距，不是智能。是因为大多数人解释不了自己的账单，不知道为什么缓存命中率突然崩了，也说不出他们的模型在第 30 个回合实际看到了什么。这个仓库就是为了把这三件事都搞清楚。

这个 Agent 正好有一个工具 —— `bash` —— 这样引擎够小，一口气能读完，而且仪表才是主角。

---

## 为什么只要一个工具

三个理由，第三个是人们经常忽略的：

- **可组合。** 你没法列举用户会需要的每一个动作，但管道把现有的工具组合起来：`grep -rl foo src | xargs wc -l | sort -n | tail -5` 是四个工具调用压缩成一次往返，中间的数据从不进上下文窗口。
- **可发现。** 模型不需要你描述环境。`ls`、`--help`、`which` 就是发现机制。
- **它继承了整个生态。** `ffmpeg`、`jq`、`rg`、`git`、`psql`、`kubectl` —— 四十年的 CLI 工具，立即可用。你不是在给 Agent 赋予工具；你是把它接到已经存在的每一个工具上。

诚实的警告，先说清楚：**"bash is all you need" 是一个关于充分性而非最优性的声明。** 真实的产品发布专用的读/编辑/搜索工具，因为它们买到了 token 效率、结构化的错误、新鲜度检查和权限细度。Bash 是阻止 Agent 被死死卡在作者能想到的东西上的办法。`docs/` 在重要的地方列举了双方的论证。

---

## 阶段

每个阶段引入恰好一个想法，并在 `stages/` 下发布一个完整的、可运行的快照。快照之间的重复是有意的 —— 在教学仓库里，可读的 diff 比 DRY 更重要。

| Stage | Idea | Status |
|---|---|---|
| [00 The Loop](docs/00-loop.md) | request → tool call → execute → repeat。一个文件，没有 SDK。 | ✅ built |
| [01 Don't Die](docs/01-dont-die.md) | 截断、超时、进程树杀死、`finish_reason`、权限闸 | ✅ built |
| [02 See Everything](docs/02-see-everything.md) | 事件总线、流式、完整仪表、JSONL trace、重放 | ✅ built |
| [03 Babel](docs/03-babel.md) | 一个 Agent，多个协议：OpenAI + Anthropic 背后是一个中立内核 | ✅ built |
| [04 The Cache](docs/04-the-cache.md) | prompt 缓存作为**纪律**，以及它值多少钱 | ✅ built |
| [05 Live Forever](docs/05-live-forever.md) | 上下文压缩、上下文注入、记忆 —— 还有上下文压缩真正的代价 | ✅ built |
| [06 The Composer](docs/06-the-composer.md) | 标准库里的 TUI：上帝视角 vs 模型视角看同一个会话 | ✅ built |
| [07 Multiply](docs/07-multiply.md) | 子 Agent（递归）、技能、什么叫真正的 PTC | ✅ built |
| [08 Sandbox](docs/08-sandbox.md) *(optional)* | 嵌入式 shell 解释器，以及为什么你没法通过读命令来保护 shell | ✅ built |

**附录：[Wire notes](docs/wire-notes.md)** —— 一个真实网关实际发出的东西，逐字节探测：每个协议怎样报告被截断的工具调用（不好，而且不一样）、流式使用数据在哪里说谎、无法识别的模型会得到什么错误码（401，不是 404）、prompt 缓存确实能工作的证明。每个声明都带上它的原始证据。`docs/` 里的教材是建立在这个文件基础上的，而不是协议文档，因为两者不一致。

## 到最后你会得到什么

- 按回合的 token 记账，能分清楚**全价 / 缓存写 / 缓存读**，还有一本用你自己货币记录的成本账目。
- 每个模型调用的 TTFT 和每秒 token；每个命令的墙钟时间。
- 一个**请求检查器** —— 按一个按键就能转储即将发送的确切字节。
- 一个 JSONL trace 记录每个会话，还有 `replay` 来单步走一个会话，**不需要 API 密钥**（这也是你怎样研究一个你从没付过钱的会话的）。
- 一个对话视图，把上下文压缩作为一流事件展示：什么被总结了、为什么、它在 token **和**缓存失效上实际花了你多少 —— 在**相同工作上的全价 token +25%** 下测量，这就是为什么第 05 阶段论证上下文压缩是一个生存机制而不是优化。
- 一个三视角 TUI，覆盖任何 trace —— **发生了什么**、**模型看到了什么**、**原始字节** —— 用标准库写的，因为 TUI 有趣的部分（原始模式恢复、Escape 歧义、显示宽度 vs 字节长度）正是框架隐藏的那些。
- 长期记忆，是 Agent 用 `>>` 追加写入的一个文件，还有一个规则来控制注入的上下文能在哪里活，这样知道时间就不会花掉你的缓存。
- 子 Agent，是同样的主循环再调用一次，并发运行进一个单一有序的 trace —— 一个重要的测量：**父 Agent 上下文小 9.6 倍却多了 20% 的 token。** 子 Agent 不省 token，它省上下文，知道你缺的是哪一个就是全部决定。
- 技能，是一个目录和一段话，索引成本就印在那儿，这样你能看到你在每个请求上永久地付的税。
- 一个嵌入式 shell，能看到每个命令**展开以后**的样子，还有一个测量表，列出 regexp 和 parser 两个都会输的十四个方式。
- 没有供应商锁定：任何 OpenAI 或 Anthropic 兼容的端点，包括本地模型，由 URL + 密钥 + 协议配置。

---

## 快速开始

需要 Go 1.24+ 和一个 POSIX shell（在 Windows 上：Git Bash，Git for Windows 附带的 —— Agent 会自动找到）。

```sh
git clone <this repo> && cd bash-is-all-you-need
cp .env.example .env      # fill in your endpoint, key and model
go build -o agent ./stages/00-loop

mkdir sandbox && cd sandbox    # it runs what the model says. use a scratch dir.
set -a && . ../.env && set +a
../agent --trace session.jsonl
> find the bug in this directory, fix it, and verify the fix
```

然后看看它干了什么 —— 不需要密钥，用别人的 trace 也能工作得一样好：

```sh
go build -o composer ./stages/06-the-composer
./composer --composer session.jsonl                  # TUI: g / m / w switch views
./composer --composer-dump session.jsonl --view model --call 12   # the same, greppable
```

任何 OpenAI 兼容的端点都能工作 —— OpenRouter、DeepSeek、Kimi、GLM、或本地的 Ollama / vLLM / LM Studio。第 03 阶段把 Anthropic 协议也加上。

---

## 非目标

讲清楚边界，因为知道一个教学项目在哪儿停止本身就是教学的一部分：

- **不是 Claude Code 的替代品。** 用 Claude Code。这个仓库讲的是原理。
- **没有 MCP、没有计划模式、没有多模型路由。** 每个文档都标注了你该在哪一层加上这些。
- **没有 Agent 框架，没有 TUI 框架。** 没有 LangChain、没有向量数据库、没有编排层、没有供应商 SDK、没有 Bubble Tea。第 00-07 阶段只有标准库加 `golang.org/x/sys` —— 第 01 阶段的 Windows Job Objects 和第 06 阶段的终端控制 —— 就这样。第 08 阶段是唯一的例外：它嵌入了 `mvdan.cc/sh/v3`，它的章节基本上就是一篇关于一个依赖到什么时候才配得上自己位置的论证，用一个测量好的账单讲那一个花了什么（它在被钉死回去之前把 Go 的底线移动过两次）。第 06 阶段是"不用框架"这条规则不再只是审美偏好的地方：TUI 框架隐藏了原始模式恢复、Escape 键歧义、显示宽度 vs 字节长度，那三件事就是这一章讲的东西。
- **不是基准追赶。** 如果你想要一个最小 Agent 的 SWE-bench 数字，看下面的 `mini-swe-agent`。

---

## 相关工作

这是个竞争激烈的领域，诚实的框架是**主循环**教得很好已经许多次了。别处缺的是仪表。

| Project | Shape | What it does not cover |
|---|---|---|
| [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) | Python，17 个渐进式课程，同样的标语 | 单供应商；没有 token/成本/缓存仪表；没有 TUI；没有重放 |
| [SWE-agent/mini-swe-agent](https://github.com/SWE-agent/mini-swe-agent) | ~100 行 Python；最纯粹的仅 bash Agent —— 它连工具调用 API 都不用，模型直接回复一个命令 | 通过 litellm 的供应商作为黑箱；不是一个渐进式课程；没有成本/缓存仪表 |
| [ghuntley/how-to-build-a-coding-agent](https://ghuntley.com/agent/) | Go，6 步讲座 | 多工具路线；没有仪表、TUI 或重放 |
| [decodingai course](https://github.com/decodingai-magazine/building-a-coding-agent-from-scratch-course) | Python，8 篇文章 + 4 个视频，Modal 沙箱 | 跟踪外包给一个 SaaS 而不是内置 |
| [owenthereal/build-your-own-coding-agent](https://github.com/owenthereal/build-your-own-coding-agent) | Python，~700 行，没有 SDK —— 精神上最接近 | 没有仪表、多协议或 TUI |

特别值得知道 `mini-swe-agent` 的是：它展示了一个比一个工具还要根本的形态 —— **零**工具，命令从纯模型输出中解析出来。如果你觉得这个仓库够小了，那个才是底线。

## 许可

MIT.
