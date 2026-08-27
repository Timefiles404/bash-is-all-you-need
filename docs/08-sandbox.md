# Stage 08 — 沙箱

*可选，也是唯一有依赖的阶段。本章大部分内容讲的是为什么这个
依赖值得加，以及它**不能**买到什么。*

---

## 一条规则

> **Agent 不能读 `.env`。**

不是"Agent 不能做危险的事情"。这么模糊的规则没法测试，而一个
测不出来的策略就是你在瞎猜。一个文件，一个动词，小到足以完全推理
——可即使这样，三个实现版本之后，诚实的答案是"这不是安全边界"。

三个角度：

```
inspectString   look at the command text            defeated by quoting
inspectAST      parse it and look at the words      defeated by expansion
sandbox.exec    BE the shell and look at the argv    see the limits below
```

这种递进不是"加更多模式"。每一层都把检查移到真实信息更多的
地方。

---

## 绕过表

十四个读这个文件的命令。每一个都**先验证能工作**，关闭策略运行它，
检查内容确实出来了——一个满是从没工作过的命令的绕过表什么都证明不了，
那是这类表最常出错的方式。由 `stages/08-sandbox/bypass_test.go` 生成，
不是凭记忆写的：

```
command                works?  string   ast    sandbox
plain                  true    blocked  blocked blocked    the command everybody blocks
single quotes          true    blocked  blocked blocked
split across quotes    true      --     blocked blocked    one word to the shell, two strings to a regexp
empty quotes inside    true      --     blocked blocked
backslash              true      --       --    blocked
leading ./             true    blocked  blocked blocked    a different string, the same file
ANSI-C quoting         true      --       --    blocked    the name never appears as text at all
variable               true    blocked    --    blocked    the value does not exist until runtime
command substitution   true    blocked  blocked blocked
eval                   true    blocked    --    blocked    the program is data until it is not
parameter default      true    blocked    --    blocked
loop                   true    blocked    --    blocked
redirect               true    blocked  blocked blocked    argv is just ["cat"] — no filename anywhere in it
nested shell           true    blocked    --    blocked    a whole program smuggled in one argument

string missed 4 · ast missed 7 · caught only by ast: 2 · caught only by string: 5
```

### 第一层被 shell 的语法本身击败

```go
var denyPattern = regexp.MustCompile(`\.env\b`)
```

`cat ".e""nv"` 对 bash 是一个单词，对这个正则是不同的字符串。
`cat .en''v`、`cat .en\v` 和 `cat $'\x2eenv'` 也是
——这里文件名根本没作为文本出现。

没有任何模式调优能修好这个，因为模式在看源文本，而 shell
在看源文本的**含义**。你不是在和攻击者竞争；你在和一门语言竞争。

### 第二层是真正的改进，还是会输

用真正的 shell 解析器来解析，就会知道 `".e""nv"` 是一个单词，其文本部分
连接起来，`'.env'` 是一个引用的字面量，而且——可喜的是——它在
`$(echo .env)` 里找到 `.env`，因为遍历树会下降到命令替换里，
在里面找到 `echo` 调用。

它不能知道的是任何还不存在的值：

```
X=.env; cat $X                the value is assigned at runtime
cat "${MISSING:-.env}"        the expansion is decided at runtime
eval "cat .env"               the program is data until it is not
for f in .env; do cat "$f"    the word is in an iterator, not an argument
```

那不是实现中的漏洞。**shell 不是语法，它是求值器，而求值器的输入
的解析树不会告诉你求值器会做什么。**

`literalWord` 返回"我不知道"而不是部分值，是故意的。返回 `.en$X`
的 `".en"` 会比返回空值更糟，因为调用者会把一个部分值和策略比较，
然后得出它是安全的。

### 杀死"就跑两个"的发现

看表的最后一行：**caught only by ast: 2 · caught only by string: 5。**
两个检查都不会互相覆盖彼此的漏洞。它们在不相交的集合上失败。

对这章明显的回应是"那就跑两个检查"。不行，因为攻击者不需要击败
一个合取。**每个命令只需要一次击败一个属性，而 shell 语法本身就
提供了选哪一个的余地。** 一行击败两个：

```sh
X=.en; eval 'cat ${X}v'
```

文本 `.env` 没出现，所以正则没什么可匹配。单词是 `${X}v`
——一个参数展开粘到字面量——所以解析器正确地说不知道。文件被读了。

---

## 第三层：做 shell

停止做局外人。嵌入解释器，它要运行的每个命令都会以一个已完成的
参数向量的形式抵达——在引用移除、参数展开、命令替换、算术、波浪
展开、通配和 `eval` 之后。没有语法可以躲在后面了，因为语法就是刚
被消费的。

```
command:  X=.en; eval 'cat ${X}v'
argv:     ["cat", ".env"]
```

```go
interp.New(
    interp.Dir(root),
    interp.StdIO(nil, &stdout, &stderr),
    interp.ExecHandlers(s.execMiddleware),   // 每个程序，最终 argv
    interp.OpenHandler(s.open),              // shell 打开的每个文件
)
```

### 为什么两个处理器，为什么第二个是惊喜

```sh
cat < .env
```

那以**没有参数**运行 `cat`。shell 打开文件并交付一个文件描述符。
一个检查 argv 的策略根本不会看到文件名——在*任何*级别，包括这个。
`OpenHandler` 是唯一能看到它的东西，没有它一个基于 argv 的策略
有一个形如 `<` 的洞。

它有自己的测试，因为它是读者最可能假设已被覆盖的情况。

### 你免费得到的另一个东西

`sandbox_exec` 对 shell 运行的**每个**程序触发，包括没人输入的：
一个管道、一个循环、一个通配或 `eval` 展开成的。这里的大部分价值
根本不是拒绝——是"那个命令实际上做了什么"成为一个有答案的问题。

```sh
agent --sandbox --observe    # report every exec and open, block nothing
```

---

## 诚实的限制

一个嵌入的解释器看到每个命令。**它看不到一个命令里面。**

```sh
awk -v a=.en 'BEGIN{f=a"v"; while((getline l < f)>0) print l}'
```

沙箱看到 `awk -v a=.en <a program>` 并对程序的内容没有意见，因为
有意见会意味着实现 awk。它允许 exec，awk 做剩下的。

那个情况是 `bypass_test.go` 里的**通过的测试**，不是遗漏：

```
--- PASS: TestTheSandboxCannotSeeInsideAProgram
    as documented: the sandbox saw one exec, allowed it, and the program did
    the rest. An embedded interpreter is a policy and observability layer, not
    a security boundary.
```

`perl`、`python`、`ruby`、`node`、`find -exec`、
`git -c core.pager=…`、`make` 和任何其他把一个程序作为参数的东西
都一样。代码明确地阻止 `sh -c` 并在注释中说这是一个无法推广的半
措施——枚举解释器是第一层已经输掉的拒绝列表游戏。

还有更多。`isSecretPath` 在基名上匹配：它不解析符号链接，不规范化
`..`，也不知道 `/proc/self/cwd/.env` 是同一个文件。即使一个做了这三
件事的版本，也还是会有竞态条件，因为一个路径可以在检查和打开之间
被替换。**TOCTOU 不是沙箱里的边界情况；它是标准攻击。**

那么这值多少？三个东西，它们不是没有：

- **可观测性。**每个 exec，展开后，在 trace 中。没有其他层能给你那个。
- **合作 Agent 的策略。**模型不是攻击者。它是一个系统，偶尔会不
  小心做出有破坏力的事，而一个捕到这种意外的层，就完成了它的工作。
- **模型可以行动的拒绝。** `blocked by the sandbox/exec policy: an
  argument resolves to .env after expansion` 是模型可以读并
  路由的一句话，这就是为什么拒绝文本是拒绝。

它不是什么：边界。一个真正的边界在 shell 下面，在操作系统——容器、
VM、独立 uid、seccomp-bpf、Landlock、mount 命名空间。这些方案要花
设置成本、牺牲可移植性，而且一旦配置错误，波及的范围也大得多，没有
一个适合进教学仓库而不变成整个仓库。**对一个编码 Agent 的正确答案
几乎总是"在容器里运行它"，而本章的层是你在容器里加的，所以你可以
看到发生了什么。**

---

## 从一个真实的运行

```
$ agent --yolo --sandbox
sandbox: commands run in the embedded interpreter (enforcing)

> Read notes.txt and tell me how many lines it has. Then try to read the .env
  file in this directory and tell me exactly what happened.

  $ wc -l notes.txt
  $ cat .env 2>&1; echo "EXIT_CODE=$?"
  ⛔ blocked by the sandbox/exec policy: an argument resolves to .env after expansion (matched ".env")
  │ 2 notes.txt
  │ [exit 0 · 64ms]
  │ <stderr>
  │ blocked by the sandbox/exec policy: an argument resolves to .env after expansion (matched ".env")
  │ [exit 1 · 1ms]
```

模型读了拒绝，理解了它，然后继续。那是整个交互设计：**一个说"拒绝"
的策略教模型重试；一个说它反对什么的策略让模型用另一种方式做任务。**

---

## 依赖，以及它实际的成本

这是唯一添加依赖的阶段，所以值得诚实地列出"添加一个依赖"意味着什么。

```
go get mvdan.cc/sh/v3@v3.13.1
  → declares `go 1.25.0`, which would raise this module's floor two releases

pin to v3.12.0
  → declares `go 1.23.0`. Fine.

...but interp imports golang.org/x/term
go get golang.org/x/term
  → upgraded golang.org/x/sys  v0.41.0 → v0.47.0
  → bumped this module          go 1.24.0 → go 1.25.0

pin x/term to v0.33.0 (`go 1.23.0`) and x/sys back to v0.41.0
  → floor restored to go 1.24.0
```

一个 `go get` 而语言版本的下限动了两次——一次是对直接依赖，一次
是对一个没人选择的传递依赖。这个仓库的每个读者都需要一个更新的工具链，
而什么都不会宣布它。

> **一个依赖的 `go` 指令是它成本的一部分**，而这部分不去看就看不
> 见。在 `go get` 之后读一遍 `go.mod`，就是全部的纪律。

让这个值得付的标准：**它做你自己做不了的东西。** 一个 JSON 解析器
你可以写。一个 TOML 解析器你可以写，这个仓库拒绝过，两次。一个 POSIX
shell 解析器*和求值器*，带展开、`eval`、算术、通配和作业控制，不是
一个你在一章里写的东西——而且这章的论证没有一个版本在没有它的情况下
能活下来，因为论证*就是*"你得做求值器"。

阶段 00-07 保持标准库加 `golang.org/x/sys`，而它们的二进制不链接这个。

---

## 练习

1. **给绕过表添加一个情况。** 测试验证你的情况在计数之前实际读了文件，
   所以一个看起来聪明但不工作的命令会响亮地失败。
2. **试试让第二层赢。** 在 `literalWord` 中处理反斜杠转义和 `$'…'` 解码，
   然后找下一个绕过。注意你在重新实现 shell 的多少，而你现在在维护第二
   个、微妙不同的 shell，其与第一个的分歧是安全 bug。
3. **把策略翻转过来**：允许列表程序而不是拒绝一个文件。然后用 `find -exec`、
   `git -c core.pager=` 和 `awk 'BEGIN{system(...)}'` 绕过它。
4. **运行一个有 `--observe` 的真实会话**并在 composer 中读 `sandbox_exec` 事件。
   数一数有多少个程序是没人输入就运行的。
5. **添加一个限制根**：拒绝工作目录外的任何 `open`。然后用符号链接打破它，
   再用一个只在展开后才存在的 `..` 打破它。然后决定是否 `EvalSymlinks`
   在每个打开之前，是你想为每个文件付的代价。
6. **测量兼容性成本。** 用和不用 `--sandbox` 运行同一个会话并找一个嵌入的
   解释器不支持的命令。它是一个非常好的 shell；但它不是你机器上的
   shell，知道是哪个差异咬了你一口，值得赶在它真正要命之前先弄清楚。
7. **做操作系统级的版本。** 一个容器，一个 `podman run --read-only` 带
   一个可写挂载，和本章的层在里面。然后比较这两个各停止什么，并注意
   它们几乎不相交。

→ 这就是课程的结束。回到 [README](../README.md)。

→ 参考：[Stage 01 — 别死](01-dont-die.md)，[Stage 07 — 乘法](07-multiply.md)
