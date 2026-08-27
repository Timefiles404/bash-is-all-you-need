# bash is all you need

**A coding agent with one tool and a glass cockpit.**

教你写 Agent 主循环的教程有一大把。在 2026 年那是一下午的活，这个仓库在
阶段 00 就做完了——一个文件，零依赖。剩下的阶段全花在没人教的那部分上：

> **盯住每一个 token、每一毫秒、每一分钱，看它到底去了哪里。**

从"我写了个 Agent 主循环"到"我能让 Agent 上生产"，差的不是智能。差的是
大多数人讲不清自己的账单，不知道缓存命中率为什么突然崩了，也说不出模型在
第 30 个回合到底看见了什么。这个仓库要做的，就是把这三件事都摆到明面上。

Agent 只有一个工具，就是 `bash`。这样引擎才小到能一口气读完，仪表也才有
资格当主角。

---

## 为什么只给一个工具

三条理由，第三条才是大家最容易漏掉的那条：

- **可组合。** 用户会需要哪些动作，你列不完；但管道能把手上现有的拼起来：
  `grep -rl foo src | xargs wc -l | sort -n | tail -5` 把四次工具调用压成
  一次往返，中间数据一次都没碰上下文窗口。
- **可发现。** 环境长什么样，不用你告诉模型。`ls`、`--help`、`which` 本身
  就是发现机制。
- **它白捡一整个生态。** `ffmpeg`、`jq`、`rg`、`git`、`psql`、`kubectl`——
  四十年的命令行积累，现成就能用。你不是在给 Agent 配工具，你是把它插进
  世上已经有的每一件工具里。

有句实话得先说在前头：**"bash is all you need" 讲的是够用，不是最优。**
真实产品都会另配专门的读、改、搜工具，因为那能换来 token 效率、结构化的
错误、过期检查和更细的权限粒度。bash 的作用，是不让 Agent 的上限卡死在
作者当初想得到的那点东西上。该争的地方，`docs/` 把两边的话都说了。

---

## 阶段

每个阶段只引入一个想法，并在 `stages/` 下留一份完整、能跑的快照。快照之间
大量重复，这是故意的：在教学仓库里，一份读得懂的 diff 比 DRY 值钱。

### 第一部分：仪表盘（00–08）

| 阶段 | 想法 | 状态 |
|---|---|---|
| [00 The Loop](docs/00-loop.md) | 请求 → 工具调用 → 执行 → 再来一遍。一个文件，没有 SDK。 | ✅ built |
| [01 Don't Die](docs/01-dont-die.md) | 截断、超时、整棵进程树一起杀、`finish_reason`、权限闸 | ✅ built |
| [02 See Everything](docs/02-see-everything.md) | 事件总线、流式、全套仪表、JSONL trace、重放 | ✅ built |
| [03 Babel](docs/03-babel.md) | 一个 Agent，多套协议：OpenAI 和 Anthropic 都藏在中立内核后面 | ✅ built |
| [04 The Cache](docs/04-the-cache.md) | prompt 缓存是一种*纪律*，以及它折成钱值多少 | ✅ built |
| [05 Live Forever](docs/05-live-forever.md) | 上下文压缩、上下文注入、记忆——以及压缩真正的代价 | ✅ built |
| [06 The Composer](docs/06-the-composer.md) | 标准库里写一个 TUI：同一个会话的上帝视角和模型视角 | ✅ built |
| [07 Multiply](docs/07-multiply.md) | 靠递归得到子 Agent、技能，还有 PTC 究竟是什么 | ✅ built |
| [08 Sandbox](docs/08-sandbox.md) *(可选)* | 内嵌 shell 解释器，以及为什么读命令字符串守不住 shell | ✅ built |

### 第二部分：生产环境（09–19）

第一部分做出来的 Agent 是*看得见*的。第二部分讲的是你把它交给别人用之后的那
一周：失败的那次调用、再也不返回的那个工具、不是 JSON 的那段 JSON、已经
过时的那条笔记、没人量过的那个 P95。规矩不变——一个阶段一个想法，没有数
字就不下结论。

第二部分接的是**阶段 07**，不是阶段 08。阶段 08 是整个仓库唯一引入依赖的
地方，而且它明说是可选的；把它带进主干，等于事实上让它变成必需。它留在
岔路上——想在后面的阶段里带上沙箱，那个补丁就是
`diff stages/07-multiply stages/08-sandbox`。

| 阶段 | 想法 | 状态 |
|---|---|---|
| [09 Triage](docs/09-triage.md) | 错误是决策，不是字符串：两套协议共用一张分类表、`Retry-After`、重试预算、降级梯子 | ✅ built |
| [10 Deadlock](docs/10-deadlock.md) | 不返回的工具和卡住的流：每一次等待都有期限，也有归属 | ✅ built |
| 11 Malformed | 工具调用不是合法 JSON——每套协议实际递给你的是什么，拿到之后怎么办 | 🚧 planned |
| 12 Echo | 最便宜的工具调用是没发出去的那次：结果按内容寻址、LRU 淘汰、用 `mtime` 判过期 | 🚧 planned |
| 13 Rewind | 会话和工作区都是状态，都得能倒回去——从 trace 续跑，改动前先打检查点 | 🚧 planned |
| 14 Amnesia | 压缩必然有损，那就把损失量出来：探针集、召回率、保护区 | 🚧 planned |
| 15 Rot | 记忆要有有效期，也要有见证人：过时不等于错、覆盖不等于矛盾、自进化的技能互相打架 | 🚧 planned |
| 16 The Briefing | 上下文共享是预算，不是布尔值——以及子 Agent 有权反问哪一句 | 🚧 planned |
| 17 Two Seconds | 盯 P95，不是盯均值：并发调用、prompt 瘦身、缓存对齐、语义缓存、模型分层 | 🚧 planned |
| 18 The Scoreboard | 从 trace 里算出四个指标，再把一个坏 case 变成回归测试 | 🚧 planned |
| 19 Borrowed Tools | 在 stdio JSON-RPC 上从零手写 MCP，并把 schema 税按 token 算出来 | 🚧 planned |

**附录：[线上记录](docs/wire-notes.md)**——一个真实网关到底发出了什么，
逐字节探过一遍：每套协议怎么报告被截断的工具调用（都报得不好，而且各报
一套）、流式返回的用量数字在哪里撒谎、模型名不存在时你会拿到哪个错误码
（401，不是 404）、prompt 缓存确实生效的证明。每条说法都带着自己的原始
证据。`docs/` 里的教材是照这份文件写的，不是照协议文档写的——因为两者
对不上。

## 走到最后你手上有什么

- 按回合的 token 记账，**全价 / 缓存写 / 缓存读**分得清清楚楚；还有一本
  成本账，用你自己的货币实时结算。
- 每一次模型调用都带 TTFT 和每秒 token 数；每一条命令都带墙钟耗时。
- **请求检查器**——按一个键，就把马上要发出去的字节原样倒出来。
- 每个会话一份 JSONL trace，配上 `replay` 就能单步走完，**不需要 API
  key**（这也是你研究一段自己没付过钱的会话的办法）。
- 对话视图把上下文压缩当成一等事件摊开：什么被总结了、为什么、它在 token
  *和*缓存失效两头各花了你多少——实测是**同样的活，全价 token 多 25%**，
  所以阶段 05 的结论是：压缩是保命手段，不是优化。
- 任何一份 trace 都能开出三个视角的 TUI——**发生了什么**、**模型看到了
  什么**、**原始字节**——用标准库写的，因为终端 UI 里真正有意思的那几处
  （原始模式怎么恢复、Escape 键的歧义、显示宽度不等于字节长度）恰好就是
  框架替你藏起来的东西。
- 长期记忆就是一个文件，Agent 用 `>>` 往后追加；再加一条规矩，管住注入
  的上下文能放在哪儿——好让"知道现在几点"不至于赔掉整个缓存。
- 子 Agent 就是同一个主循环再调一次，并发跑，汇进同一条有序的 trace——
  还有那个真正要紧的数：**多花 20% 的 token，换来小 9.6 倍的父上下文。**
  子 Agent 不省 token，它省的是上下文；你缺的到底是哪一样，决定就在这儿。
- 技能就是一个目录加一段话，索引成本直接印出来，让你看见这笔税要在往后
  每一个请求上一直交下去。
- 内嵌的 shell 看到的是每条命令*展开之后*的样子，还有一张实测表，列齐了
  正则和解析器双双翻车的十四种方式。
- 一张失败分类表，把两套协议的错误收成三种决策——重试、降级、停下——依据
  是实录的字节，而那些字节正好把两条看上去最显然的规则推翻了：模型名不
  存在返回 **401**，请求体格式不对返回 **500**。还有那个没人报的数字，
  因为 API 根本问不出来：**失败的那些尝试花了多少钱**。
- 流式调用上三个时钟，而不是一个：`http.Client.Timeout` 管的是响应体读取，
  所以它分不出"答得慢"和"socket 死了"——再加上每条流实际出现过的最长静默，
  **直接打在面板上**，就在 TTFT 旁边，于是那个超时是量出来的余量，不是
  谁看着顺眼的数。实测：14 次调用里最长的静默是 **5.0 秒**，默认值 45 秒；
  而量到这个数用了三次，因为前两次量的都是别的东西。
- 不锁供应商：任何 OpenAI 或 Anthropic 兼容的端点都行，本地模型也算，用
  URL + key + 协议三样配出来。

---

## 快速开始

需要 Go 1.24+ 和 POSIX shell（Windows 上就用 Git for Windows 自带的 Git
Bash，Agent 会自己找到它）。

```sh
git clone <this repo> && cd bash-is-all-you-need
cp .env.example .env      # 填上你的端点、key 和模型
go build -o agent ./stages/00-loop

mkdir sandbox && cd sandbox    # 它会执行模型说的话。用一个临时目录。
set -a && . ../.env && set +a
../agent --trace session.jsonl
> 在这个目录里找出 bug，修掉它，然后验证修好了
```

然后回头看它干了什么——不需要 key，看别人的 trace 和看自己的一样顺手：

```sh
go build -o composer ./stages/06-the-composer
./composer --composer session.jsonl                  # TUI：g / m / w 切换视角
./composer --composer-dump session.jsonl --view model --call 12   # 同样的内容，可以 grep
```

任何 OpenAI 兼容的端点都能跑——OpenRouter、DeepSeek、Kimi、GLM，或者本地
的 Ollama / vLLM / LM Studio。阶段 03 会把 Anthropic 协议并排加上来。

---

## 非目标

把边界说出来——教学项目在哪里收手，本身也是教学的一部分：

- **不是 Claude Code 的替代品。** 要用就用 Claude Code，这个仓库只负责把
  它讲明白。
- **没有计划模式。** 每篇文档都标出了它该加在哪一层。MCP 和多模型路由在
  阶段 08 之前一直是非目标，到第二部分才来——阶段 19 和阶段 17——而且各自
  带着自己的账单来，不是当成一条功能亮点写上去。
- **不用 Agent 框架，也不用 TUI 框架。** 没有 LangChain、没有向量数据库、
  没有编排层、没有厂商 SDK、没有 Bubble Tea。阶段 00-07 和第二部分的每一个
  阶段都只有标准库，外加 `golang.org/x/sys`——阶段 01 拿它做 Windows Job
  Objects，阶段 06 拿它做终端控制——就这些。阶段 08 是唯一的例外：它内嵌
  了 `mvdan.cc/sh/v3`，那一章基本上就是在辩一个依赖凭什么配得上自己的
  位置，并附上一笔实测的账：就这一个依赖，花掉了多少（在被钉回去之前，
  它把 Go 的版本底线抬高过两次）。到了阶段 06，"不用框架"就不再只是审美
  偏好：TUI 框架会替你藏掉原始模式恢复、Escape 键的歧义、显示宽度和字节
  长度的区别，而这一章讲的正好就是这三件事。
- **不追榜。** 想看最小 Agent 能跑出什么 SWE-bench 分数，看下面的
  `mini-swe-agent`。

---

## 相关工作

这条赛道上人不少，公道地讲：*主循环*这件事已经有很多人讲得很好了。别处
缺的是仪表。

| 项目 | 形态 | 它没覆盖的 |
|---|---|---|
| [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) | Python，17 节递进课程，标语一样 | 只认单一供应商；没有 token / 成本 / 缓存仪表；没有 TUI；没有重放 |
| [SWE-agent/mini-swe-agent](https://github.com/SWE-agent/mini-swe-agent) | ~100 行 Python；最纯粹的"只有 bash" Agent——它连工具调用 API 都不用，模型直接回一条命令 | 供应商全交给 litellm，是个黑箱；不是递进课程；没有成本 / 缓存仪表 |
| [ghuntley/how-to-build-a-coding-agent](https://ghuntley.com/agent/) | Go，6 步工作坊 | 走多工具路线；没有仪表、没有 TUI、没有重放 |
| [decodingai course](https://github.com/decodingai-magazine/building-a-coding-agent-from-scratch-course) | Python，8 篇文章 + 4 个视频，Modal 沙箱 | trace 外包给 SaaS，不是自己做出来的 |
| [owenthereal/build-your-own-coding-agent](https://github.com/owenthereal/build-your-own-coding-agent) | Python，~700 行，不用 SDK——精神上最接近 | 没有仪表、没有多协议、没有 TUI |

`mini-swe-agent` 值得单独说一句：它演示的形态比"只有一个工具"还要极端——
*零*工具，直接从模型的纯文本输出里把命令解析出来。你要是觉得这个仓库已经
够精简了，那一个才是地板。

## 许可

MIT.
