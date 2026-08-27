# 第 07 阶段——乘法

两个功能，都不是子系统。

```
subagents   a fresh []Msg, a different system prompt, the same everything
            else — and only TEXT comes back.
skills      a directory of Markdown files, and one paragraph saying they exist.
```

没有调度器、没有消息队列、没有 Agent 注册表，也没有 Agent 之间的协议。
子 Agent 是一个函数调用，返回值是一段文字。

标题，因为它与人们通常的理解相反：

> **子 Agent 不省 token。它省的是上下文。**

下面有实测：委托运行花了**20% 更多**，父 Agent 的上下文最后**小 9.6 倍**。
这两个数字方向相反，知道自己缺哪一个才是决定何时委托的全部内容。

---

## 子 Agent 就是再次调用的同一个主循环

```go
func (a *agent) spawn(callID, description, prompt string) (string, Usage, error) {
    child := a.newChild(agentID, func() string { return subagentSystem + para + a.stable })
    msgs := child.runTurn([]Msg{TextMsg(RoleUser, prompt)})
    return lastAssistantText(msgs), child.spent, nil
}
```

这就是这个功能。父 Agent 已经有了一个主循环、一个总线、一个压缩器和一个权限闸；
子 Agent 是同一个主循环，用了不同的消息数组。什么被共享、什么不被共享，
结果成了唯一有趣的设计问题：

| shared | not shared |
|---|---|
| provider, HTTP client | the message array |
| the permission gate | the system prompt |
| the shell config and working directory | the compactor |
| the **bus core** — one ordered trace for the whole tree | the turn budget |

使这值得做的条款是 `lastAssistantText`。子 Agent 做的其他所有事——每一个工具调用、
每一个 40kB 的命令输出、每一个它退出的错误回合——都活在一个消息数组里，那个数组被丢弃了。
父 Agent 的上下文只会增长报告的长度，其他什么都不增。

子 Agent 在系统提示词中被明确告知这一点，而不是被留去自己推断：

> 你在这里做的一切在你完成后都被丢弃了，**除了你最后的消息**。
> 你的调用者永远看不到你的命令、你的推理或你的工具输出——只看最后你说的话。

没有那一段，子 Agent 会写出它的**流程**总结——"我看了几个文件，找到了一些东西"——
因为那是聊天回合通常的样子。被明确告知最后的消息是唯一幸存下来的，它写一份报告。

---

## 来自真实运行：委托实际花了什么

三个 Markdown 文件总共 63kB。一个任务：完整阅读每一个并写一份三句话的总结。
两个分支，同一批文件，同一个模型：

```sh
agent --yolo --max-output 60000              # delegate: one subagent per file
agent --yolo --max-output 60000 --max-depth 0  # inline: the task tool does not exist
```

```
  ╭─ subagent · Summarize wire-notes.md
  ╭─ subagent · Summarize 02-see-everything.md
  ╭─ subagent · Summarize 03-babel.md
  │ $ cat .../wire-notes.md
  │ $ cat .../02-see-everything.md
  │ $ cat .../03-babel.md
  ╰─ 2 turns ·  5133 prompt +  204 output tokens ·  6327ms →  707B returned
  ╰─ 2 turns · 11952 prompt +  237 output tokens · 12441ms →  827B returned
  ╰─ 2 turns ·  4696 prompt +  380 output tokens · 13519ms →  950B returned
```

| | 委托 | 内联 |
|---|---:|---:|
| 模型调用 | 9 (3 父 Agent，6 子 Agent) | 3 |
| prompt token 数 | 25,782 | 19,715 |
| 以 0.1x 缓存读 | **22,038** | **18,390** |
| output token 数 | 1,635 | 571 |
| 实际运行时间 | 39s | 18s |
| **结束时的父 Agent 上下文** | **1,893** | **18,160** |

委托花了 20% 更多的全价当量 token、2.9 倍的 output token、
2.2 倍的实际运行时间。它也结束于一个父 Agent，其整个上下文是 1,893 个 token——
因为那 63kB 的 Markdown 被三个 Agent 各读了一遍，其中没有一个还在持有这段对话。

再看那三行 `╰─`。**21,781 个 prompt token 进去了，2,484 字节出来了。**
那个比例就是这个产品。其他一切都是为它付出的开销。

所以决策规则不是"这个任务是否足够大到委托"。而是：

> 委托**读很多东西、结论很少**的工作，当这个读不是你之后还会用到的东西时。
> 如果你需要中间的输出，子 Agent 严格比内联做完更差——你为 token 付了两次钱，
> 然后把结果丢了。

这也是为什么 `task` 工具的描述是用经济学而不是机制写的。
说工具**做什么**的描述，模型完全不知道什么时候去用它。

---

## 并发，和一个提前三章到期的账单

三个子 Agent 同时跑。注意 trace 仍然是一个、完全有序的流——
每个事件都有一个 `Seq`，确定它相对于树里每个其他事件的顺序，跨越每个 Agent。

这不是新工作。第 02 阶段选择了一个同步的总线，在一个锁下，
为了排序，当时只有一个生产者，没有明显的理由在乎：

```go
type busCore struct {
    mu   sync.Mutex
    seq  int
    subs []Subscriber
}

func (b *Bus) Fork(agent string) *Bus {
    return &Bus{core: b.core, depth: b.depth + 1, agent: agent}
}
```

`Fork` 什么都不复制。一个计数器，一个锁，N 个生产者。一个异步的
单订阅者总线——那个"扩展性更好"的设计——会给每个订阅者一个不同的并发会话故事，
那正是你没有一个统一故事就无法推理的会话。

每个 Agent 一份 trace 是另一个常见的选择，它让你真正有的唯一问题——
**子 Agent 跑的时候父 Agent 在做什么？**——只有通过在时间戳上合并文件才能回答，
那正好是时间戳擅长的反面。

### 渲染器退化而不是撒谎

三个子 Agent 把 token 流进一个终端光标会产生一段从三个不同句子组合起来的段落。
它不仅只是丑陋；它读起来就像一个 Agent 在自相矛盾。

所以简朴的渲染器展示子 Agent 的**结构**——它跑了什么、花了什么、返回了什么——
然后丢掉散文。什么都没有丢失,因为每个 delta 都在 trace 里，
第 06 阶段的 composer 正是因为线性终端对树来说形状不对而存在。

> **无法显示某个东西的渲染器应该通过显示更少来表示，永远不是通过错误地显示它。**

### 并发执行，确定性历史

`dispatch` 并发地跑子 Agent，**按照模型要求它们的顺序**返回结果。
如果结果是当它们完成时追加的，同一个会话重放两次会产生两个不同的消息数组、
两个不同的 prompt 前缀，以及——按第 04 阶段——一个从不命中的缓存。

> 并发可以改变事情花多长时间。它不被允许改变对话说什么。

出于同一个原因，权限闸也用上了一个 mutex，而且它比看起来更尖锐。
两个 goroutine 写一个 prompt 到一个终端会产生一行交错的文字，
然后读一个答案给两个问题：**用户批准了一个命令，一个不同的命令运行了。**
那是一个穿着 UI bug 衣服的安全 bug。`dispatch` 在任何并发开始前
在一个 goroutine 上问每一个问题，所以锁应该永远不竞争——
而它在那里是因为"应该永远"不是你想要一个权限闸依靠的属性。

---

## 深度保险丝，和为什么工具消失了

在深度限制处，`task` 工具**从工具列表中移除了**，不是在调用时被拒绝。

一个运行时拒绝花一个完整的往返——模型写一个调用，宿主拒绝它，
模型读拒绝并尝试别的——它花工具定义的 token 在每一个无法用它的请求上。
更坏的是，它是一条模型看得出是武断的规则，而模型会用改措辞来和武断的规则争论。

一个不在列表里的工具不是一个规则。没有什么可以争论，没有什么可以绕过，
模型在它有的工具内计划。

---

## 技能是一个目录和一段文字

```
skills/
  mutation-test/SKILL.md    ---  name: … / description: … ---  then the body
  new-stage/SKILL.md
  wire-probe/SKILL.md
```

系统提示词得到了名字和一行描述。体留在磁盘上。模型判断某个技能适用时，
就用 `cat` 读它。没有技能工具，没有检索步骤，没有运行时——
那是第 05 阶段关于记忆的观察，从另一个方向到达：
一旦 Agent 有了一个 shell，"当相关时加载这个文档"不是你要构建的功能，
它是一个文件名。

承重的是形状。**渐进式披露**：索引总是，体在需要时。

```
  ≡ skills: 3 skills · index 738B in every request · 6.1kB of bodies left on disk
```

那个数字是有目的地打印的，因为索引**不是免费的**，算术是整个设计决策。
在会话的整个生命周期里，738 字节都待在每个请求的前缀中——
第 04 阶段之后，虽然能以十分之一的价格缓存，但永远不会是零。
四十个技能就是几千个 token 的永久开销。
如果没有人修剪，技能目录只会不断增长，这就是一种税，征收在 Agent 做的每一次调用上，
唯一能让人注意到的办法，是有什么东西把这个数字打印出来。

索引中的三个指令，每一个因为这如何出现错误的方式：

- **"行动前先读体"**——否则模型根据描述行动，那是一行长且是为了**可选择性**写的，
  而不是为了充分。
- **"最多一个"**——一个模型给了五个看似都合适的技能，会读所有五个，
  那会把一个 token 储蓄变成一个 token 花费加五个往返。
- **"如果都不适用，忽略这个列表"**——没有它，一个技能列表读起来像
  一个模型被期望订购的菜单，它会找到一个几乎合适的。

frontmatter 解析器是二十行而不是 YAML 依赖。当你拥有接口的两端时，
解析器可以做得跟接口一样小。

---

## PTC 真正是什么

程序化工具调用的卖点只有一个：模型写代码调用工具，代码在别处运行，
**中间结果永远不进入上下文**。

一个 shell 管道已经做了那个。同一个任务，三种方式——**在 `src/` 下找出五个最大的 Go 文件**——
在 21 个文件上：

| | 模型调用 | 命令 | prompt token | 以 0.1x 读 | 工具输出入上下文 | 实际 |
|---|---:|---:|---:|---:|---:|---:|
| 每文件一个调用 | 25 | 23 | 51,937 | 8,161 | 1,255 B | 80s |
| `wc -l src/*.go` | 2 | 1 | 2,097 | 1,175 | 509 B | 12s |
| `find … \| sort -rn \| head -6` | 2 | 1 | **1,608** | **686** | **157 B** | **8s** |

**同一个答案。32 倍的 prompt token，10 倍的实际运行时间。**

读"工具输出入上下文"列，因为那是机制而不是症状。
管道的 `sort | head` **在 shell 内**做了它的过滤，
所以进入上下文的是 157 字节，不是 1,255。中间数据存在；它只是永远没有变成 token。

两件事值得从表中拿走。

**中间这一行是个意外，却恰好证明了这一点。**那个分支被认为是慢的——
指令说没有管道、没有链接、没有 `xargs`、没有 `find`。模型用 `wc -l src/*.go` 回答了，
完全遵守且在一个调用中做了整个工作，因为**一个 glob 已经是一种批操作**，
而这里面没有用到任何一个我禁止的运算符。免费得到扇出、不用开口要，是这个仓库
从它的开头部分就一直在声称的 shell 的属性，而这一点全靠一次失败的实验才让它可见。

**那么 PTC 在管道上加了什么？**不是标题好处——那一个是一个 `|`。
它加的是一个真实的编程语言而 shell 只有组合：
有类型的工具 API 而不是文本流，对结构化结果进行条件判断和循环，
错误处理不是 `$?`，工具不是一个 PATH 上的程序。
如果你的工具是 CLI 程序，而你的控制流是"过滤、排序、取"，
那么管道就是 PTC，你从 1973 年起就已经有它了。
如果你的工具是带 schema 的 HTTP API，而你的控制流有分支，那就不是了——
这个差异，值得专门用一个运行时。

---

## 完全没有 `task` 工具的版本

一个能跑 bash 的 Agent 可以跑**Agent**：

```sh
agent --subagent "survey every provider adapter and report the disagreements"
```

一个 prompt 进，一个报告出，没有 REPL。
这是一个完整的子 Agent 机制，用的只是 Agent 已经有的工具，
值得看看这背后其实没有多少东西——递归不需要编排层，它需要的是一个进程。

它付出的代价，是仪表板上的一切。一个单独的进程有它自己的总线，
所以它的事件不在你的 trace 中；它的 token 不在你的账本中；
它的权限 prompt 会跟你的抢终端；
它的失败是一个退出码而不是停止原因。
这一切你都得重新造一遍——架在一个管道上，用一种你自己还得再做版本管理的格式。

那是整个进程内/进程外选择的诚实总结：
**shell 一直是个相当好的编排器，直到你想知道它到底花了什么为止。**

---

## 练习

1. **再现两个分支**并比较每个中的最后 `context` 数字。比例就是你在买什么。
2. **委托一件你需要中间输出的事**——"找到 bug 并给我显示 diff"——
   并看报告是无用的。子 Agent 的失败模式不是它错了，
   而是**它在一个你没有选择的方向上是有损的**。
3. **从 `subagentSystem` 中删除"一切都被丢弃"那段话**，然后读三份报告。
   数一数有多少份描述的是过程而不是发现。
4. **让 `dispatch` 当它们完成时追加结果**而不是按索引。跑同一个会话两次并 diff
   两个 trace 的请求体。
5. **设置 `--max-depth 3`**并给 Agent 一个足够大的任务来递归。
   然后算出你最坏情况下的账单是多少，以及代码里是否有任何保险丝能拦住它。
6. **加第四个技能并再次测量索引。**外推到四十。决定你的修剪策略
   **在你需要它之前**。
7. **在一个大到能看出差别的目录上自己跑一遍 PTC 表格**，找到单命令版本的输出
   撑不下 `--max-output` 的那个点。那个数字，就是 shell 不再够用、
   而一个真实沙箱开始成为答案的地方——那就是第 08 阶段。

→ Next: Stage 08 — Sandbox

→ Reference: [Stage 04 — The Cache](04-the-cache.md), [Stage 05 — Live Forever](05-live-forever.md), [Stage 06 — The Composer](06-the-composer.md)
