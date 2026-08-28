# bash is all you need

[English](README.md) · **中文**

**一个只有一个工具的编程 agent，和一套能看清它在干什么的仪表盘。**

---

## 这是什么

教你写一个 agent 循环的教程有很多。在 2026 年，那确实是一下午的工作，这个仓库第 00 章就写完了 —— 一个文件，346 行，没有任何依赖。

剩下的十二章讲的是没人教的那部分：

> **每一个 token、每一毫秒、每一分钱，到底花到哪里去了。**

"我写过一个 agent 循环"和"我能让一个 agent 跑在生产环境里"之间的差距，不是模型不够聪明。是大多数人解释不了自己的账单，说不清缓存命中率为什么突然掉了，也回答不出第 30 轮的时候模型究竟看到了什么。这个仓库就是为了让这三件事变得可见。

agent 只有 `bash` 一个工具，这样引擎本身可以小到一口气读完，仪表盘才有位置当主角。

---

## 为什么只给一个工具

三条理由，第三条最容易被忽略：

- **能组合。** 你没办法预先列举用户需要的每一个动作，但管道能把已有的组合起来：`grep -rl foo src | xargs wc -l | sort -n | tail -5` 是四次工具调用压缩成一个来回，中间数据一个字节都不进上下文窗口。
- **能自己摸索。** 你不需要向模型描述它所在的环境。`ls`、`--help`、`which` 就是探测手段。
- **它接管了一整个生态。** `ffmpeg`、`jq`、`rg`、`git`、`psql`、`kubectl` —— 四十年的命令行工具，立刻可用。你不是在给 agent 配工具，你是把它接到了所有已经存在的工具上。

先把话说在前面：**"bash 就够了"是一个关于「够用」的主张，不是关于「最优」的主张。** 真正的产品都会另外提供读文件、改文件、搜索的专用工具，因为那能换来更省的 token、结构化的错误、文件是否过期的检查，以及更细的权限粒度。bash 的价值在于，它让 agent 的能力上限不等于作者的想象力上限。该讲两面的地方，每一章都会讲两面。

第 01 章有一个直接戳到这条主张的测量：权限闸门只能把一个命令字符串原样显示给你看，它没法显示一个 diff，没法告诉你这次 `git push` 要推到哪儿，也没法检查文件是不是过期了 —— 因为在它眼里 `grep` 和 `git push` 长得一模一样。

---

## 课程

十三个阶段，每个阶段一个根目录：

```
00-loop/
  code/      那个阶段完整、可直接编译运行的程序
  doc/       那一章，和它引用的图
```

阶段之间大量重复代码，这是**故意的**。把相邻两个目录 diff 一下，看到的正好是一个新想法，不多也不少 —— 一个读得懂的 diff，比 DRY 更重要。

表格第二列写的是**那一章要解决的问题**，不是那一章有什么功能。这门课是按问题串起来的：每一章的结尾，就是下一章的开头。

### 第一部分 —— 仪表盘（00–08）

| 阶段 | 这一章面对的问题 |
|---|---|
| [00 循环](00-loop/doc/README_zh.md) | 模型知道下一步该做什么，但它够不着你的机器。于是你成了那只手，在两个窗口之间复制粘贴。 |
| [01 别死](01-dont-die/doc/README_zh.md) | 它会执行 `find /` 把几百 MB 灌进上下文；会挂在 `npm run dev` 上永不返回；Ctrl-C 之后进程还留在系统里；也会因为你说了"清理一下"就执行 `rm -rf .`。 |
| [02 看见一切](02-see-everything/doc/README_zh.md) | 它跑了 40 秒，做了四次调用，花了不知道多少钱，而你只看到最后一段文字。 |
| [03 巴别塔](03-babel/doc/README_zh.md) | 上一章造的仪表盘，每一个字节都是照着 OpenAI 的形状写的。而你手上的端点说的是 Anthropic。 |
| [04 缓存](04-the-cache/doc/README_zh.md) | 第 00 章那张表：一次会话为 4982 个 token 付了钱，而对话本身最后只有 1192 个。而且它是二次增长的。 |
| [05 活下去](05-live-forever/doc/README_zh.md) | 缓存让重发变便宜了，但没让窗口变大。第 30 轮，请求根本装不下了。 |
| [06 作曲家](06-the-composer/doc/README_zh.md) | 每次会话都留下几十 MB 的 JSONL。"第 30 轮模型看到了什么"这个问题，你没法靠翻它来回答。 |
| [07 分身](07-multiply/doc/README_zh.md) | 一个上下文，几件互不相干的事。它们互相污染，而且谁也塞不下。 |
| [08 沙盒](08-sandbox/doc/README_zh.md)（可选） | 你没办法靠读命令字符串来给一个 shell 加上安全边界。 |

### 第二部分 —— 生产环境（09 起）

第一部分造出一个你**看得见**的 agent。第二部分讲的是你把它交给别人用之后的那一周：失败的调用、永不返回的工具、不是 JSON 的 JSON、过期的笔记、没人测过的 P95。规矩不变 —— 一章一个想法，没有数字就不下结论。

第二部分从**第 07 章**接着走，不是第 08 章。第 08 章是整个仓库唯一引入外部依赖的地方，被标成可选；把它放进主干，那个依赖就变成必需的了。它是一条岔路 —— 想在后面的阶段里用上沙盒，`diff 07-multiply/code 08-sandbox/code` 就是那个补丁。

| 阶段 | 这一章面对的问题 |
|---|---|
| [09 分诊](09-triage/doc/README_zh.md) | 调用失败了。重试吗？这是哪一种失败？两种协议对同一件事的说法还不一样。 |
| [10 死锁](10-deadlock/doc/README_zh.md) | 一切正常，只是什么都不返回。工具不返回，流也停在半路。 |
| [11 畸形](11-malformed/doc/README_zh.md) | 模型交回来的工具调用不是合法 JSON。修它是个陷阱。 |
| [12 回声](12-echo/doc/README_zh.md) | 同一条命令你付了两次钱 —— 而在动手写缓存之前，先用手上已有的 trace 算清楚它到底值多少。 |
| 13 倒带 | 会话和工作区都是状态，都需要一个"退回上一步" |
| 14 失忆 | 压缩是有损的，那就把损失量出来 |
| 15 腐烂 | 一条记忆需要有效期，也需要一个证人 |
| 16 简报 | 共享上下文是一笔预算，不是一个布尔值 |
| 17 两秒 | 要看 P95，不是平均值 |
| 18 记分牌 | 从 trace 里取四个指标，把一个坏 case 变成回归测试 |
| 19 借来的工具 | 从零写一个 MCP，把 schema 税用 token 数出来 |

**附录：[wire notes](external/wire-notes.md)** —— 一个真实网关到底发了什么，逐字节探测出来的：两种协议怎么报告一个被截断的工具调用（都报得很糟，而且方式不同）、流式统计在哪里说谎、模型名写错时返回的是 401 而不是 404，以及缓存确实生效的证据。每一条结论都带着原始证据。每一章的教学内容都建立在这个文件上，而不是建立在协议文档上 —— 因为这两者对不上。

---

## 快速开始

需要 Go 1.24+ 和一个 POSIX shell（Windows 上用 Git Bash，装了 Git for Windows 就有，agent 会自己找到它）。

```sh
git clone <this repo> && cd bash-is-all-you-need
cp .env.example .env      # 填入你的端点、key 和模型

go build -o agent ./00-loop/code

mkdir sandbox && cd sandbox    # 它会执行模型说的任何命令。用一个空目录。
set -a && . ../.env && set +a
../agent --trace session.jsonl
> 找出这个目录里的 bug，修好，然后验证你修对了
```

这就是第 00 章：一句提示词，一个循环，别无他物。

**从第 06 章开始，同一个二进制会打开一个交互式的界面** —— 一个可以折叠细节的回滚区、一个带边框的输入框，下面一行显示provider、模型、上下文用了多少、这次会话花了多少钱；Escape 中断当前这一轮，Ctrl-O 收起或展开仪表盘，以及一组斜杠命令：

```sh
go build -o agent ./12-echo/code
cd sandbox && ../agent
```

`/help` 列出全部，`/keys` 是键盘，`/status` 打印当前这次会话的全部配置。什么都没配也能启动：`/provider-url`、`/provider-protocol`、`/provider-model`、`/provider-apikey`、`/provider-window` 可以现场配好一个端点并保存到仓库外面 —— 这就是为什么直接双击这个二进制也能用。`/open <dir>` 把 agent 换到另一个目录。

之前能用的方式一个都没坏：

```sh
../agent -p "说明一下这个目录是干什么的"    # 跑一轮，不开界面，退出
echo "同一件事" | ../agent                  # 管道，跟第 00 章一样
../agent --no-tui                           # 各章里那个朴素的命令行提示符
```

那个交互界面在 `external/tui/`，**没有任何一章讲它**，这是刻意的 —— 原因写在 [AGENTS.md](AGENTS.md) 里。

然后回头看它到底干了什么。不需要 key，别人的 trace 也一样能看：

```sh
go build -o composer ./06-the-composer/code
./composer --composer session.jsonl                # 界面里 g / m / w 切换三个视角
./composer --composer-dump session.jsonl --view model --call 12   # 同样的东西，可以 grep
```

任何 OpenAI 兼容的端点都能用 —— OpenRouter、DeepSeek、Kimi、GLM，或者本地的 Ollama / vLLM / LM Studio。第 03 章会在旁边加上 Anthropic 协议。

---

## 不做什么

把边界说清楚，因为知道一个教学项目在哪里停下来，本身就是教学的一部分：

- **不是 Claude Code 的替代品。** 要用就用 Claude Code。这个仓库是用来解释它的。
- **没有 plan mode。** 每一章会指出你该在哪一层加它。
- **没有 agent 框架，也没有 TUI 框架。** 没有 LangChain，没有向量数据库，没有编排层，没有厂商 SDK，没有 Bubble Tea。00–07 和第二部分的每一个阶段都只用标准库加 `golang.org/x/sys`（第 01 章的 Windows Job Object，第 06 章的终端控制），仅此而已。第 08 章是唯一的例外：它内嵌了 `mvdan.cc/sh/v3`，而那一章主要就是在论证一个依赖什么时候配得上它的代价 —— 包括它两次抬高了 Go 版本下限这件事。

  第 06 章是"不用框架"这条规矩从审美变成实质的地方：TUI 框架恰好藏起了三样东西 —— raw 模式的恢复、Escape 键的歧义、显示宽度和字节长度的区别 —— 而那一章讲的就是这三样。
- **不刷榜。** 想看一个极简 agent 的 SWE-bench 分数，看下面的 `mini-swe-agent`。

---

## 相关项目

这是一个拥挤的领域，实话是：**循环**这件事已经有很多人讲得很好了。别处缺的是仪表盘。

| 项目 | 形态 | 它没有覆盖的 |
|---|---|---|
| [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) | Python，17 节递进课程，同一句口号 | 单一供应商；没有 token / 成本 / 缓存的测量；没有 TUI；没有回放 |
| [SWE-agent/mini-swe-agent](https://github.com/SWE-agent/mini-swe-agent) | ~100 行 Python；最纯粹的 bash-only agent —— 它连 tool-calling API 都不用，模型直接回一条命令 | 供应商经由 litellm，是个黑盒；不是递进课程；没有成本 / 缓存测量 |
| [ghuntley/how-to-build-a-coding-agent](https://ghuntley.com/agent/) | Go，6 步工作坊 | 多工具路线；没有仪表盘、TUI 或回放 |
| [decodingai course](https://github.com/decodingai-magazine/building-a-coding-agent-from-scratch-course) | Python，8 篇文章 + 4 个视频，Modal 沙盒 | 追踪外包给了 SaaS，而不是自己造 |
| [owenthereal/build-your-own-coding-agent](https://github.com/owenthereal/build-your-own-coding-agent) | Python，~700 行，无 SDK —— 精神上最接近 | 没有仪表盘、多协议或 TUI |

`mini-swe-agent` 值得单独说一句：它示范了一种比"只有一个工具"更激进的形态 —— **零个**工具，命令直接从模型的纯文本输出里解析出来。如果你觉得这个仓库已经够精简了，那个是地板。

---

## 许可

MIT。
