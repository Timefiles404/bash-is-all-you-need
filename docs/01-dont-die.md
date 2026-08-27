# 阶段 01 — 活下来

阶段 00 工作得很好，直到它不能工作为止。这一章讲四种具体的失败方式，
以及每种的修复成本。

**先做复现**。下面每个修复看完都很明显，但没看之前都不明显。
花十五分钟破坏阶段 00 会给你直觉，使得阶段 01 读起来像必然而非武断。

```sh
go build -o agent00 ./stages/00-loop
go build -o agent01 ./stages/01-dont-die
mkdir -p sandbox && cd sandbox
```

---

## 死法 1 — 永不返回的命令

**复现**（阶段 00）：

```
> start a simple http server on port 8000 in the foreground
```

Agent 运行 `python -m http.server 8000` 再也回不来。没有超时、没有
值得一提的 Ctrl-C 处理、除了杀死终端没有出路。

**显而易见的修复，以及为什么它不够**。加一个超时，当它触发时
杀死 `cmd.Process`。现在试试：

```
> run: (sleep 300 &) ; echo started
```

Shell 立即退出。`echo started` 打印了。Agent 仍然永远挂起。

这是让人惊讶的部分。`cmd.Wait()` 不是在等进程——它在等*标准输出
管道关闭*，只要任何进程持有写端，管道就会保持打开。后台的 `sleep`
继承了那个句柄。所以只杀死 shell 不只是泄漏孤儿：**它挂起了本该救你
的超时本身**。

**真正的修复**是让 shell 和它生成的一切成为一个单元可杀死：

- **Unix** — `SysProcAttr{Setpgid: true}` 把 shell 放入新进程组，
  `kill(-pgid)` 信号整个组。
- **Windows** — 一个 Job Object；每个后代都分配给它，`TerminateJobObject`
  一次结束所有的。

那是 `proc_unix.go` 和 `proc_windows.go`，在一个小接口后面，所以
`runBash` 从不提到平台：

```go
g, _ := newProcGroup()
defer g.Close()
g.attach(cmd)          // 在 Start 之前：进程组 / job 创建标记
cmd.Start()
g.adopt(cmd)           // 在 Start 之后：分配给 job（Windows）
...
g.kill()               // 整个树都下来
```

**通用化的教训**：当清理路径依赖你在清理的东西时，它不是一条
清理路径。检查每个"然后我们杀死它"是否有这种形状。

**然后把它应用到修复本身**。`g.kill()` 应该解除阻塞
`Wait()`——但"应该"就是我们刚看到失败的同样假设。所以收割得到自己的
五秒截止，如果那过期了 Agent 会报告 `[TIMED OUT and could not be reaped]`
然后继续。

在那里放弃会泄漏 `Wait` goroutine，它持有输出缓冲区，所以代码还必须
**拒绝读这些缓冲区**——复制 goroutine 可能仍在写入它们，这时候硬要
去读就是数据竞争，数周后会在上下文里冒出乱码。泄漏一个 goroutine
是可以活下来的；卡住 Agent 不是；竞争缓冲区是三者中最糟的，因为
它默默地失败。

### 从一个真实的运行

准确的复现，通过完成的 Agent，带五秒超时：

```
> run exactly this: (sleep 300 &) ; echo started ; sleep 300

  $ (sleep 300 &) ; echo started ; sleep 300
  | started
  | [TIMED OUT after 5.046s — the process tree was killed]
```

四件事要注意，都是设计决策的回报：

- **它回来了**。整个交换的十八秒墙时钟，其中五个是超时。阶段 00
  仍会运行。
- **`started` 活了下来**。杀死前产生的输出仍然被捕获和显示。超时
  不是扔掉你学到的东西的理由。
- **模型被告知，用文字，发生了什么**——它的总结正确地解释了
  后台 sleep 随树一起死亡。状态行是写给读者看的——因为它确实有
  读者。
- **零孤儿**。`ps -W | grep -c sleep` 前后都读 0。那个数字，就是
  这一整节存在的全部目的。

### 为什么 Unix 这里密不透风，Windows 不是

值得内化，因为它解释了代码的形状：

- **Unix** 在 `fork()` 和 `exec()` 之间设置进程组——子进程在**它的
  第一条指令运行前**就在自己的组中。什么都逃不掉，因为什么都没执行。
- **Windows** 不能把 Job Object 分配给还不存在的进程。所以 `adopt()`
  在 `Start()` *之后*运行，有一微秒窗口，shell 的第一个行为生成的
  孙辈会逃出 job。

密不透风的 Windows 修复是 `CREATE_SUSPENDED` + `ResumeThread`，`os/exec`
故意搞它复杂（一个 Go 的收割者永不恢复的暂停进程，会挂起 `Wait()`）。
这个 repo 接受这个窗口，**在代码中记录它**，而不是假装它不存在。
如果你在写沙箱而不是教学 repo，直接驱动 `CreateProcess`，旗标和线程
句柄都在 `PROCESS_INFORMATION`。

### 你实际上会遇到的平台注意事项

| 注意事项 | 为什么重要 |
|---|---|
| **`Close()` 不对称** | Windows `KILL_ON_JOB_CLOSE` 意味着关闭句柄*杀死*树——一个真正的崩溃安全网。Unix 没有：如果 Agent 死了，它的子进程活在 init 之下。所以 `nohup npm start &` 在 Linux/macOS 上活下来，在 Windows 上**不是**。 |
| **嵌套 job 需要 Win8+** | 一些 CI 运行程序和容器主机已经把每个进程放在锁定的 job 中；`AssignProcessToJobObject` 然后失败并带 `ERROR_ACCESS_DENIED`。`runBash` 降级为警告——命令仍运行，隔离失效——而不是拒绝工作。 |
| **僵尸看起来活着** | `kill(pid, 0)` 对未收割的僵尸成功。通常 init 立即收割；在 PID 1 不收割的容器中，它们逗留。轮询，不检查一次。 |
| **PID 回收这里不是风险** | `os.Process` 从 `Start()` 到 `Wait()` 持有一个开放的句柄，Windows 在句柄开放时不会回收 PID。 |

### 只在 Windows 上存在的陷阱：`$!` 撒谎

发现于写这章的测试中，值得一段因为它会默默地使整个事情无效。

Git Bash 是 MSYS2，它维护**自己的 POSIX PID 命名空间分层在 Windows PID 上**。
`echo $!` 打印 MSYS pid，不是 Windows 的：

```
msys_pid=48908                                <- what $! prints
48908 48907 48905  56176 ... /usr/bin/sleep   <- ps -W: the real WINPID is 56176
```

交 48908 给 `OpenProcess` 它不报错——它欢快地查询碰巧拥有那个数字的
不相关的 Windows 进程。建立在 `$!` 之上的*测试*看起来通过了，其实
什么也没证明；建立在它之上的*杀手*，杀的是旁观者。

翻译在 `/proc/<pid>/winpid`，所以夹具用 `cat /proc/$p/winpid 2>/dev/null || echo $p`
——MSYS2 回答，真实 Unix 没有这样的文件并回到已经正确的 pid。

更大的教训，不只关乎 Windows：**当你的测试和实现共享一个假设时，
测试不能发现假设是错的**。这就是为什么这个测试套件里也特意放了
一个演示这种失败模式的测试（`TestProcGroupKillingOnlyTheShellLeavesOrphans`），
以及为什么这份实现经过了变异测试——`TerminateJobObject` 被替换成
no-op——来确认测试在代码破损时实际失败：

```
proc_test.go:209: orphans survived kill(): [18592 36592] — the process tree escaped
--- FAIL: TestProcGroupKillsWholeTree (5.22s)
```

一个你从不见它失败的绿色测试不是证据。

---

## 死法 2 — 打印 40MB 的命令

**复现**（阶段 00）：

```
> how many files are on this machine?
```

模型尝试 `find / -type f | wc -l`——没问题。现在看它想看*名称*时
会尝试 `find / -type f`。阶段 00 把每个字节都推进消息数组，此后
一直留在那里，在每个后续回合都重发一次、重新计费一次。

**修复：截断，但不是从前面**。只从头截断是本能，它是错误的本能
——失败构建的有用部分是*最后*二十行。保留两端，丢掉中间，说你丢了
多少：

```
<first 2/3 of the budget>

[... 1481923 bytes elided ...]

<last 1/3 of the budget>
[exit 0 · 3.2s] [output truncated — rerun with a filter such as grep/head/tail]
```

`truncate()` 的三个细节比看起来更重要：

- **在 rune 边界切**。在字符中间切片字节数组产生无效的 UTF-8，
  一些 API 完全拒绝它，其他把它变成模型上下文的乱码。
- **说字节数**。"某东西被移除"对模型远没有"1.4MB 被移除"有用
  ——后者告诉它命令根本是错误的形状。
- **告诉它改怎么做**。后缀命名 `grep`/`head` 能测量地减少
  模型重试同一转储的次数。

**预算分割**。标准输出得 ⅔ 的预算，标准错误得 ⅓。一个失败的构建，
标准输出只有一点，标准错误却是一大堆；换成列表命令，情况正好
相反。分割预算意味着两者都不会挨饿。

### 从一个真实的运行

一个 275KB 的日志，其*最后一行*是唯一重要的：

```
> cat big.log and tell me what went wrong at the end

  $ tail -100 big.log
  | [... 1503 bytes elided ...]
  | NFO  worker-008 processed batch 003976 in 396ms
  | 2026-08-27T02:00:00 ERROR worker-042 FATAL: disk quota exceeded, aborting
  | [exit 0 · 161ms] [output truncated — rerun with a filter such as grep/head/tail]
```

仔细读读那段，因为它把"头+尾"这整个论证都摆在了一个屏幕里：模型
要的是一百行，还是被截断了，**而 FATAL 行活了下来**，因为它在
尾部。只从头截断的话，会交付 5KB 的例行 INFO 行，却把用户问的那
一行丢了——模型还是会照着拿到的东西自信地回答。

注意 `NFO  worker-008` 在上面的行：那是尾部从半行中间续上，
有点丑但完全无害。不要花代码让截断漂亮。

这次运行还显示了两件事：

- 系统提示词的*"倾向于过滤命令而非转储命令"*做了真实的工作
  ——模型用的是 `tail -100`，从不是 `cat`。便宜的指令击败昂贵的
  机制。
- 在截断通知后，它跟进了 `tail -20` **和** `grep -c 'error\|fatal'`
  **在单个助手消息中**——一次两个 `tool_calls`。并行工具调用在这个
  供应商上不是假设的，这正是"每个调用得一个结果，总是"是规则
  而不是锦上添花的原因。

---

## 死法 3 — 模型被截断

阶段 00 问每个响应一个问题：*有工具调用吗？* 那一个问题就藏住了
一整类失败。

**复现**：发一个请求带一个微小的 `max_tokens` 和需要长命令的任务。
这是实际回来的，来自同一网关的两个协议。

**OpenAI 这边是诚实的失败**。`max_tokens: 24`：

```json
{"finish_reason": "length",
 "message": {"content": null, "tool_calls": null,
             "reasoning_content": "The user wants to search for Go files containing the word \"deadline\" in the"}}
```

在推理中被截断，所以从不发出工具调用。注意阶段 00 在这里会讲的谎
是什么形状：它看不到工具调用，推断"回合完了"，打印空消息并等你。
什么都没崩溃。什么都没报告。任务就停了。

**Anthropic 这边是危险的**。`max_tokens: 10`：

```json
{"stop_reason": "tool_use",
 "usage": {"output_tokens": 136},
 "content": [{"type": "tool_use", "name": "bash", "input": {"raw_arguments": ""}}]}
```

短短五行里，错了三个地方：

1. **`stop_reason` 说 `tool_use`**。不是 `max_tokens`。信封声称这
   是一个常规的、可用的工具调用。
2. **`max_tokens` 没被遵守**。十个被要求；136 个被生成。在这个
   网关一个小的 `max_tokens` 不是成本上限，你不应该围绕它计划
   预算。
3. **`input` 不是你发布的模式**。必需的 `command` 键缺席；一个
   非规格 `raw_arguments` 键持有空字符串。

### 这在 Go 里产生的 bug，看起来不像 bug

```go
var args struct{ Command string `json:"command"` }
json.Unmarshal([]byte(`{"raw_arguments":""}`), &args)  // err == nil
args.Command                                           // ""
```

解组**成功**。Go 用零值填充缺席的键，所以"模型省了一个必需字段"
和"模型发了空字符串"变成了同一个值，Agent 就运行了空命令，还
以为自己真是被这样要求的。

修复是一个字符宽：

```go
var args struct{ Command *string `json:"command"` }   // 指针，不是值
```

`nil` 现在意味着缺席，`""` 意味着空，两者都被拒绝。那是 `main.go`
的 `parseBashArgs`，`render_test.go` 喂它这个网关实际被观察产生的
六个有效负载。

**两条要记住的规则，都比这个 bug 更大：**

- **解组不报错，不代表验证过**。对照你发布的模式去验证——每次都要，
  每个协议都要。
- **信封证明不了它装的内容**。`stop_reason` 由产生畸形体的同一个
  系统生成。两者不一致时，会在你机器上运行的是那个体。

**半个 shell 命令不是更安全的 shell 命令**。阶段 01 拒绝执行
任何来自 `length` 终止的响应的东西，并告诉模型为什么，在一个工具结果中，
所以它可以用更短的东西重试：

| `finish_reason` | 意思 | 阶段 01 做什么 |
|---|---|---|
| `tool_calls` / `tool_use` | 常规工具回合 | 执行 |
| `stop` / `end_turn` | 模型完成讲话 | 结束回合——但如果工具调用无论如何存在，相信调用，不是标签 |
| `length` / `max_tokens` | 中途生成被截断 | **不**执行。用解释回答每个待决调用然后让它重试 |
| `content_filter` | 供应商阻止了 | 报告并结束回合 |
| 其他任何 | 新的或厂商特定 | 报告字面字符串并结束回合。永不默默地把未知状态当作成功 |

最后一行是值得保留的习惯：一个映射未知输入到"可能没问题"的
状态机最终会映射拒绝、配额事件或新的安全停止到"可能没问题"。

---

## 死法 4 — 你不想要的命令

阶段 00 里，模型和 `rm -rf` 之间什么都没有。修复是一个闸，
有趣的部分不是 prompt——而是**否定**是什么。

```go
case deny:
    msgs = append(msgs, toolResult(call.ID,
        "[the user denied this command. Do not retry it unchanged.]"))
    continue
```

一个否定是**数据，不是错误**。它作为工具结果回去，回合继续，
模型得提议更窄的东西。把拒绝当作致命错误，会在一个人正专注
盯着看的那一刻杀死 Agent——这恰恰是失去线的最坏可能时刻。

模式：`y` 一次，`a` 为会话，`n` 否定并继续，`q` 停止。`--yolo`
完全跳过闸。如果标准输入是一个管道，就没人可问，所以闸提前
检测出这一点并明说，而不是默默地否定一切。

### 从一个真实的运行

不带 `--yolo`，把任务用管道传进去，于是每个命令都被拒绝：

```
> list the files here
  $ ls -la
  [denied: no terminal to ask on — rerun with --yolo to allow commands]
  $ ls
  [denied: no terminal to ask on — rerun with --yolo to allow commands]

It looks like both `ls -la` and `ls` were denied. Could you let me know which
command or approach you'd prefer me to use to list the files?
```

这就是这套设计换来的行为。Agent 被拒绝，**缩小了它的提议**
（`ls -la` → `ls`），又被拒绝了一次，然后问了一个明智的问题——
一切在同样的回合中，因为一个否定是一个工具结果而不是致命错误。
换成从那条路径返回 `error`，你得到的就是一份堆栈跟踪和一个死掉
的会话。

### 对"bash 是所有你需要"的诚实论证

看闸能向你显示什么：一个命令字符串。那是它有的全部。

一个专用的 `write_file` 工具可能渲染一个**diff**。一个专用的
`send_email` 工具可以把**收件人**显示给你看。一个专用的 `edit`
工具，可能在文件自从模型上次读取以来发生过变化时拒绝写入——
一个 bash 根本表达不了的不变量。并且像 `grep` 这样的只读工具
可能被标记为并行安全，而 `bash -c "..."` 不管是 `grep` 还是
`git push`，都是同样不透明的形状，所以宿主必须把一切都**串行化**。

这是真实的权衡。一个工具廉价地买广度。专用工具给宿主能力
**闸、渲染、审计和并行化**——你为那广度每次想问用户一个好问题
都付费。repo 其余部分为一个工具停止因为仪器是主题；一个产品
会提升三或四个行为并保留 bash 作为逃生舱口。

---

## 清理三人组

命令输出还不是文本。三个分开的问题全部表现为"怪字符"：

| 问题 | 症状 | 修复 |
|---|---|---|
| ANSI 转义 | `[0;32m` 在上下文中乱扔；浪费 token | 用正则表达式去掉 |
| CRLF | 看不到的 `\r` 在每个 Windows 行上 | 正常化到 `\n` |
| 无效 UTF-8 | 一个本地程序在本地代码页写（中文 Windows 上是 GBK，日文 Windows 上是 Shift-JIS） | 用 U+FFFD 替换无效字节 |

第三个值得停留，如果你在非英文 Windows：字节不是损坏的，
它们*在不同的编码中正确*。替换它们使失败**可见**而非无声
——模型看 `����` 并知道出错了，而不是自信地推理乱码。
真实转码是 `golang.org/x/text/encoding`，故意不是这里的依赖；
`chcp 65001` / `PYTHONIOENCODING=utf-8` 路由在源处修复它。

---

## 它花了什么？

整章是关于四个失败的，修复它们的代码小于解释它们的代码。
那个比例对宿主工作来说很正常，这就是宿主工作被低估的原因：
这些东西没有一样能让 Agent 更聪明，这一切都是一个演示和一个
工具之间的区别。

这章之后故意仍失踪：

- 你仍为整个模型回合盯着一个空白终端（**阶段 02**）。
- 你仍不能以任何有用的形式看 token 账单（**阶段 02**，然后
  **04**）。
- 仍没有发生什么的记录（**阶段 02**）。
- 历史仍永远增长（**阶段 05**）。

---

## 练习

1. **复现管道挂**。只超时 `cmd.Process`，不是树，然后运行
   `(sleep 300 &) ; echo hi`。看 `cmd.Wait()` 无论如何阻塞。这是
   章中单个最有价值的十分钟。
2. **只从头截断**并给它一个失败的构建。注意错误消息——唯一重要的
   部分——正好是被丢掉的。
3. **删除否定文本中"不要原样重试它"这句话**并否定什么。
   看模型多少次提议相同命令。工具结果措辞是 prompt 工程。
4. **设 `--timeout 1s`** 并问一些慢的。确认模型读 `[TIMED OUT]`
   行并适应而不是重复自己。
5. **把标准输入接上管道，不带 `--yolo` 运行它**。确认你得一个
   清晰的消息而不是拒绝的默默墙。

→ 下一步：阶段 02 — 看一切 *(in progress)*

→ 参考：[线上笔记](wire-notes.md) — 这章的每个声明所基于的观察行为
