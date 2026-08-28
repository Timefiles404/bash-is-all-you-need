# 阶段 12 · 第 3 部分：接进循环，然后把它默认关掉 —— 一次查询放在哪儿，以及面板为什么要打印 0

[00](../../00-loop/doc/README_zh.md) → 01 → 02 → 03 → [04](../../04-the-cache/doc/README_zh.md) → 05 → 06 → 07 → 08 → 09 → [10](../../10-deadlock/doc/README_zh.md) → [11](../../11-malformed/doc/README_zh.md) → `12`

> [返回本章目录](README_zh.md) · 上一部分：[什么算同一条命令](2-witness_zh.md)

---

## 问题

缓存写好了。接进去的改动是三行：在跑命令的那个函数顶上查一次，命中就返回，不命中照旧跑完再存。跑一遍，它工作了。

然后你会发现这三行替你做了几个决定，而且没有一个能从"缓存应该更快"推出来。

第 5 轮的时候你批准过 `cat notes.md`。第 40 轮模型又发了同一条，缓存里有答案 —— **还要不要再问你一次？** 不问，看起来只是省掉一次多余的确认。

命中的时候没有进程启动过。而屏幕上那个命令计数、回放文件头上那个"N 条命令"、以后任何人写的统计，全都在读 `command_start` 和 `command_end` 这两个事件。**命中的时候，trace 该说什么？** 顺手也发一对，代码上是多写半行的事。

模型这一次拿到的是当初存下来的那段文本，尾巴上带着 `[exit 0 · 92ms]`，而这次调用其实只花了几微秒 —— **要不要把这个数改成真的？** 最后还有一个：审计说这个功能值万分之四，**它要不要默认打开？**

这些问题都不在"这个功能能不能工作"的范围里。**接一个功能进系统，改的是三行；定下来的是这个系统还说不说真话。**

---

## 办法

四个决定，各自选了一边。

| 决定 | 选的那一边 | 因为 |
|---|---|---|
| 查询和权限闸门谁在前 | 闸门在前，查询在后 | 命中也是字节到模型眼前，"你见过"不是"你可以见" |
| 命中要不要发命令事件 | 一个都不发，只发一条 `result_cache` | 没有进程启动过。一份说有进程跑过的 trace 不是证据 |
| 面板上零命中要不要打印 | 打印 | 零命中和正常工作长得一模一样 |
| 默认开还是关 | 关 | 万分之四 |

前三行是原则，第四行是量出来的。这一部分先把前三行做出来，再把第四行的数字摆上桌。

---

## 怎么做的

### 第 1 步：查询放在闸门之后

派发那一段，闸门先问，问完才走到跑命令的函数里：

```go
v, why := a.g.ask(command)
a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
switch v {
// ...
default:
	texts[i] = a.runCommand(ctx, turn, c.ID, command)
}
```

把查询挪到闸门**前面**，理由听起来也很像样：既然答案已经有了，第二次就没必要再打扰用户。不成立的地方在于，一次命中同样是几千个字节出现在模型眼前，而"这条命令你以前批准过"和"这条命令现在可以再来一次"是两件事。真那么接，一对命令里的第二条就**变成不可否决的了** —— 第一次点了"允许"，之后同样的命令再也不会问你。一个会话越长就越难拦住东西的权限系统，是往错的方向漂。

所以有一条测试专门盯着这个顺序：

```go
if got := rec.count(KindGateVerdict); got != 2 {
	t.Fatalf("gate verdicts = %d, want 2: the second command was served without being asked about", got)
}
```

### 第 2 步：命中就返回，一个命令事件都不发

```go
look := a.echo.lookup(a.cfg.shell, a.cfg.wd, command, a.cfg.maxOutput, a.cfg.env)
if look.verdict == cacheHit {
	a.bus.Emit(Event{
		Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
		Verdict: string(cacheHit), Bytes: len(look.text), Millis: look.millis,
	})
	return look.text
}
```

一条 `result_cache`，然后返回。没有 `command_start`，没有 `command_end`。

理由短得几乎不像一个决定：**没有进程启动过，也没有进程结束过，凭什么发这两个事件。** 顺手发出来是半行的工作量，代价是这个仓库里每一份开着缓存录的 trace，都变成一份说了"有一个进程跑过"的文件 —— 而那个进程不存在。

```go
if got := Summarize(rec.events).Commands; got != 1 {
	t.Errorf("the replay header reports %d commands, want 1", got)
}
if got := Summarize(rec.events).CacheHits; got != 1 {
	t.Errorf("the replay header reports %d cache hits, want 1", got)
}
```

两条命令、一个进程、一次命中。回放文件头上那一行也照这个说：

```text
trace · 103 events · 2 turns · 1 command · 24.203s · 2 cached
```

这份 trace 来自一段真实会话：模型被要求读一个文件，然后两次核对自己读到的东西，于是发出了**三条一模一样的工具调用**，其中**两条从来没有变成进程**。

### 第 3 步：另外三个判决也各发一条，但只在缓存开着的时候

```go
if a.echo != nil {
	a.bus.Emit(Event{
		Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
		Verdict: string(look.verdict), Text: look.reason,
	})
}
```

miss、stale、refused 都进 trace，带着理由。屏幕上只印命中 —— 一个冷缓存全是 miss，一段干活的会话大半是 refused，把它们都念一遍，命令输出会被记账信息埋掉。而"这段会话为什么一次都没命中"这个问题，是你专门打开 `--replay` 去查的，那里四个判决全在。`a.echo != nil` 的意思是：缓存没开的时候，这个事件一条都不该有。

### 第 4 步：先发事件，再存

```go
a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
r := runBash(ctx, a.cfg.shell, command, a.cfg.timeout)
rendered, truncated := r.render(a.cfg.maxOutput)
a.bus.Emit(Event{
	Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
	ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
	Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
})
a.echo.store(look, command, rendered, r)
```

存放在最后，而且它的失败不影响上面任何一行 —— 它多数时候是失败的，第 2 部分那些拒绝规则就是干这个的。trace 记的是发生过的事：一条命令跑过了就该被记下来，跟它有没有被存进缓存无关。交给 `store` 的是**整个 `look`**，不只是那个 key —— 它身上带着这些见证人在命令读它们之前的哈希，第 2 部分第 6 步那次前后比较靠它。

### 第 5 步：交回去的必须是原原本本那几个字节 —— 连那个已经不成立的耗时

存进去的是 `rendered`，也就是模型当初读到的那段文本，包括结尾这一行：

```go
status := fmt.Sprintf("\n[exit %d · %s]", r.ExitCode, r.Duration.Round(time.Millisecond))
```

第 1 部分已经问明白了重复是怎么来的：模型重发同一条命令，是因为**先前那个结果已经不在它眼前了**。所以命中时不能用一句"你在第 2 轮跑过这条命令"打发它 —— 那句话指的正是不存在的那个东西。要给的就是原来那 5,447 个字节。

```go
if results[0] != results[1] {
	t.Errorf("the cached result differs from the one the command produced:\n first: %q\nsecond: %q",
		results[0], results[1])
}
```

而"原原本本"把那个耗时也一起带过来了。模型被告知这次调用花了 92 毫秒，实际上它花了几微秒。

这一处**违反了这一章自己的规矩**，而且是故意违反的。trace 那一侧守住了：没有进程，就不报进程。给模型的那一份让掉了：把耗时改成真数，两次同一条命令拿到的文本就不一样了，而"两次完全一样"是这个功能唯一的正确性标准。这个取舍摆在这里，比让读者自己撞上去好。

### 第 6 步：子 agent 和父 agent 共享同一份缓存，按指针

```go
echo: a.echo,
```

判断标准是"这是谁的事实"。"这个端点在拒绝调用"是关于端点的事实；"这个文件里是这些字节"是关于**工作区**的事实，而父 agent 和它每一个孩子同时在看同一个工作区。一个每人一份的缓存是正确的，但它会漏掉这个功能唯一明显划得来的场景：三个孩子在同一秒里打开同一个文件。

```go
child := a.newChild("kid", func() string { return "sys" })
if child.echo != a.echo {
	t.Fatal("the child got a different result cache; three children reading the same file would " +
		"miss on every one of them")
}
```

这一行是因为第 10 章丢过一次才写的：那次 `dl` 没有被列进这个结构体，于是子 agent 带着全部超时都关着的状态在跑，而没有任何地方说过这件事。

### 第 7 步：关掉的那条路是一个分支，不是第二个实现

```go
if rc == nil {
	return cacheLookup{verdict: cacheRefused, reason: "cache disabled"}
}
```

```go
if rc == nil || look.key == "" {
	return
}
```

缓存上每一个方法都受得住空接收者，这样"没开缓存"就只是 `a.echo` 是 nil，而不是另写一份 `runCommand`。两份实现的问题不在于多写代码，在于以后每一个改动都得改两遍，而漏掉的那一遍不会有人发现 —— 因为默认走的正是不开缓存那条路。

```go
if got := rec.count(KindResultCache); got != 0 {
	t.Errorf("result_cache events = %d with the cache off, want 0", got)
}
```

缓存关着的时候，这个 agent 和第 11 章那个完全一样。

### 第 8 步：面板上零也要打印

这个仓库其他地方的规矩是反着的。重试次数是零就不印：

```go
if r.retries > 0 {
	word := "retries"
	if r.retries == 1 {
		word = "retry"
	}
	r.p("  %d %s\n", r.retries, word)
}
```

一段会话每一轮都印一行 `retries: 0`，读者两天就学会不看这个区块了。缓存这一行是唯一的例外：

```go
if lookups := r.cacheHits + r.cacheMisses + r.cacheStale + r.cacheRefused; lookups > 0 {
	r.p("  result cache: %d hits / %d lookups (%d refused · %d stale) · %s not re-read · %s not re-run\n",
		r.cacheHits, lookups, r.cacheRefused, r.cacheStale,
		humanBytes(r.cacheBytes), time.Duration(r.cacheSaved)*time.Millisecond)
}
```

"零次重试"值得藏起来，因为一段没有重试的会话就是一段没出事的会话。"零次命中"值得印出来，因为一个从不命中的缓存看起来**和一个正常工作的缓存完全一样** —— 没有报错，没有错答案，日志里一行都没有 —— 而唯一能把两者分开的就是这一行：

```text
  result cache: 0 hits / 12 lookups (0 refused · 0 stale) · 0B not re-read · 0s not re-run
```

README 里那条 15 秒过期时间的事 —— 那是别处的经验，这个仓库没有它的 trace —— 是这一行存在的全部理由。

### 第 9 步：默认关

```go
useCache    = flag.Bool("cache", false, "serve a repeated read-only command from a result cache instead of running it")
```

```go
if *useCache {
	a.echo = newResultCache(*cacheMax, *cacheBytes, *cacheTTL)
}
```

这个仓库里前十一章的功能都是默认开的。这一章是例外，理由在下面。

---

## 跑一下

```sh
go build -o agent ./12-echo/code

cd sandbox
set -a && . ../.env && set +a
../agent --cache --trace t12c.jsonl
```

试这两句：

1. `开三个子 agent，让它们分别从 wire-notes.md 里找出所有 JSON 字段名、所有 HTTP 状态码、所有小节标题`
2. `/status`，看 `stage 12` 那一段

跑完之后，把刚录下来的这份 trace 喂给第 1 部分那个审计工具：

```sh
../agent --cache-audit t12c.jsonl
```

**观察重点：**

- 第 1 句里，三个子 agent 的命令挤在同一段时间里，`[cached · 5.4kB, not run]` 出现在原本该出现命令输出的位置，而下面没有耗时那一行。
- `/status` 里 `hits` 那一行的 Note 写着"多少毫秒的命令时间没有花掉"。`refused` 那一行写着"不合格，而且永远不会合格"。
- 最后那次审计报出来的命中数是 **0**，而屏幕上那一行刚刚说了 12 次。两个数都是对的，下面 量一量 解释这件事。
- `../agent --replay t12c.jsonl` 里那些 `cache_hit` / `cache_refused` 行。屏幕上只印过命中，这里四个判决都在。

---

## 量一量

### 能构造出来的最好情况

四个 agent（一个父亲，三个孩子）同时被要求读同一个文件：

```text
  38 calls · 44 commands
  prompt tokens billed: 499172  (full 80484 · write 0 · read 418688)
  result cache: 12 hits / 56 lookups (2 refused · 0 stale) · 63.8kB not re-read · 1.107s not re-run
```

重复长这样，同一个文件的同一段，被两三个不同的孩子各读一遍：

```text
List every JSON field name#1   sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '1,300p' wire-notes.md      5449B   99ms
List every HTTP status code#3  sed -n '301,600p' wire-notes.md    5447B   83ms
List every HTTP status code#3  sed -n '601,952p' wire-notes.md    5448B   90ms
List all section headings#2    sed -n '301,600p' wire-notes.md    5447B   83ms
List all section headings#2    sed -n '601,952p' wire-notes.md    5448B   90ms
```

| | |
|---|---|
| 整段会话墙上时间 | 4 分 23 秒 |
| 模型时间 | 360,656 ms |
| 真的花掉的命令时间 | 3,789 ms |
| 因为缓存没花掉的命令时间 | 1,107 ms |

56 次查询 12 次命中，命中率 **21%**，砍掉了 **22.6%** 的命令时间。这是这一章能构造出来的上限。

换成整段会话：**模型时间的 0.3%，墙上时间的 0.4%。**

### 同一次会话里，另一个缓存做了两百倍的活

```text
  prompt tokens billed: 499172  (full 80484 · write 0 · read 418688)
```

**418,688 / 499,172 = 83.9% 的 prompt token 由第 [04](../../04-the-cache/doc/README_zh.md) 章那个供应商侧的提示缓存供上了**，按十分之一收费。

那个缓存不需要白名单，不需要见证人，不需要分词器，也不需要这一部分的九个决定。第 04 章为它写了一个 HTTP 头。

![十秒 shell，十四分钟模型](images/time_zh.svg)

上面这张图是十六段 trace 的形状，和这一段的结论是同一个：**一个 agent 里真正贵的重复发生在线上，不在 shell 里。** "我在重复跑同一条命令"这个直觉，指的是两个缓存里错的那一个。

### 这个缓存一个 token 都没省

命中交回去的是同样的字节，进同一份对话记录。就在上面那段会话里，**四个 agent 各自收到了同一段文本自己的那份 5,449 字节拷贝 —— 而缓存正在命中。**

看起来显然的改进（重复就回一个指针）是走不通的：模型重发这条命令，恰恰是因为先前的结果已经不在它眼前了。

**结果缓存是一个省墙上时间的功能，不是一个省 token 的功能。** 这句话第 1 部分说过一次，这里是它的实测形式。

### 那次 A/B 什么都没证明，而这一节就是来说这件事的

同一个任务，关掉缓存再跑一遍：**47** 条命令（开着的时候 44 条），模型调用少了三次，输出 token 多了 **2,300** 个，中间撞上一个 500 赔了一次重试，总共 **4 分 50 秒**对 **4 分 23 秒**。

看起来缓存赢了 27 秒。它没有。那 27 秒里有一次 500 重试，而缓存声称的效果是 **1.107 秒** —— 噪声比信号大两个数量级。

**唯一算证据的是缓存自己数的那个计数器。** 一个功能的效果小到 A/B 测不出来的时候，你要么承认测不出来，要么去量那个功能自己做了多少次动作。第二条路诚实，第一条路不诚实。

### 审计指向的那个场景，没有重现

第 1 部分算出来的四次命中，两次来自压缩之后的重读。听起来这就是这个功能该发光的地方。

**这一章为它跑的三段真实会话，没有一次因为压缩而命中。** 其中一段紧到压缩了五次，它把一个文件分十二段顺着读完，**一段都没有重读** —— 因为摘要里记下了哪些范围已经读过，而模型信了。

一个好的压缩器，会把这个缓存造出来要吸收的那些重复先消掉。

### 那个工具看不见自己的成功

上面 跑一下 最后一步的两个数：审计报 0，面板报 12。

机制在这里：**审计只数 `command_end`，而一次命中不发 `command_end`。** 这不是巧合，是第 2 步那个决定的直接后果 —— 命中的时候没有进程，trace 就不说有进程。

同一个任务的两次运行，一次关缓存录、一次开缓存录：

| | 命令条数 | 审计报出的命中 | 缓存自己数到的命中 |
|---|---:|---:|---:|
| 关着缓存录的 | 47 | **9** | —— |
| 开着缓存录的 | 44 | **0** | **12** |

第二行那个 0 不代表缓存没工作，它当场服务了 12 次。那 12 次在 trace 里的痕迹是 12 条 `result_cache`。

这个 bug 不能修，因为它不是 bug：修它就意味着让 trace 报告没有发生过的进程。能做的只有把用法写清楚 —— **审计要拿关着缓存跑出来的 trace 做**，一旦打开，唯一的证据就是缓存自己打印的那个计数。

再往上抽一层，这句话不只关于这一个工具：**一个靠"数被省掉的那件事"来评估某功能的工具，在这个功能真的生效时读数为零。** 被省掉的东西不会在记录里留下自己的名字。你自己造下一个"避免做某件事"的功能时，衡量它的仪表也会有同一个盲区，而它读出来的零看上去和"这个功能没用"一模一样。

---

## 接下来

这一章的结论是"不打开"。

三段量出来的数字都指向同一边：十六段 trace 上是万分之四，最好的情况是模型时间的 0.3%，而同一次会话里第 04 章那个缓存供上了 83.9% 的 prompt token。一个回报这么低的功能，应该是你知道自己的负载之后去打开的东西，而不是读者一声不响就继承下来的东西。

那么写它值不值？值，但值的地方不是这个功能。

值的是那个动作：**把已经发生过的事重放一遍。** trace 里有命令，所以能在写之前算出缓存值多少；trace 里有 token 数，所以能算出压缩贵不贵。这一部分还添了一条：**当一个功能的效果小于噪声的时候，能证明它的只有它自己的计数器。**

还有一条留给下一个人：这一章造出来的"见证人"，是一个能说"你记的那件事已经不成立了"的东西。第 05 章那个 `MEMORY.md` 里的每一行，现在都在被无限期地相信。

[回到本章目录](README_zh.md) 看后面几章接手的是哪些问题。
