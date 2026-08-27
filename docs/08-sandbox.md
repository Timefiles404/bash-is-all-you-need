# 阶段 08：沙箱

*可选，也是全仓库唯一带依赖的阶段。这一章大半篇幅都在讲：那个依赖凭
什么配得上它的位置，以及它**买不到**什么。*

---

## 一条规则

> **Agent 不能读 `.env`。**

不是"Agent 不能干危险的事"。这么含糊的规则根本测不了，而测不了的策略
就是你在瞎猜。一个文件，一个动词，小到能从头到尾想明白——可即便如此，
三套实现写完，诚实的回答仍然是"这不是安全边界"。

三个可以站的位置：

```
inspectString   看命令的文本                被引号击败
inspectAST      解析它，看解析出的单词      被展开击败
sandbox.exec    自己就是 shell，看 argv     极限见下文
```

这不是"再多加几条模式"式的递进。每往下一层，检查就挪到一个能看见更多
真相的位置。

---

## 那张表

十四条能把这个文件读出来的命令。每一条都**先验证过确实有效**：把策略
关掉跑一遍，确认内容真的出来了——一张绕过表，要是里面的命令根本就没跑
通过，它什么也证明不了，而这正是这类表最常出错的地方。表是
`stages/08-sandbox/bypass_test.go` 生成的，不是凭记忆写出来的：

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

### 第一层输给 shell 自己的语法

```go
var denyPattern = regexp.MustCompile(`\.env\b`)
```

`cat ".e""nv"` 在 bash 眼里是一个单词，在这个正则眼里是另一个字符串。
`cat .en''v`、`cat .en\v`、`cat $'\x2eenv'` 也都一样——最后这个里面，
文件名根本没以文本的形式出现过。

再怎么打磨模式都修不好这件事，因为模式看的是源文本，而 shell 看的是源
文本*代表什么*。你不是在跟攻击者较劲，你是在跟一门语言较劲。

### 第二层是真的进步，但照样输

拿真正的 shell 解析器去解析，它知道 `".e""nv"` 是一个单词、字面部分会
拼起来，知道 `'.env'` 是带引号的字面量；而且——这点挺让人高兴——它能在
`$(echo .env)` 里找到 `.env`，因为遍历语法树会走进命令替换，在里面碰上
那个 `echo`。

它没法知道的，是任何此刻还不存在的值：

```
X=.env; cat $X                值是运行时才赋上去的
cat "${MISSING:-.env}"        展开成什么，运行时才定
eval "cat .env"               一直是数据，直到它不是
for f in .env; do cat "$f"    这个词在迭代器里，不在参数里
```

这不是实现留下的漏洞。**shell 不是一套语法，它是求值器；而求值器输入
的语法树，不告诉你求值器会干什么。**

`literalWord` 宁可返回"我不知道"，也不返回半个值，这是故意的。对
`.en$X` 返回 `".en"` 比什么都不返回更糟：调用方会拿这半个值去跟策略
比对，然后得出"安全"的结论。

### 那个把"两个都跑"判死的发现

看那张表的最后一行：**caught only by ast: 2 · caught only by string:
5。** 两个检查各自漏掉的东西，谁也不是谁的子集。它们失败在互不相交的
集合上。

读到这里最自然的反应是"那就两个检查都跑"。行不通，因为攻击者面对的
不是合取式。**每条命令只需要一次击败一个性质，而 shell 的语法正好让
它挑着来。** 一行就把两个都干掉：

```sh
X=.en; eval 'cat ${X}v'
```

`.env` 这几个字一次都没出现，正则无从匹配。那个单词是 `${X}v`——参数
展开后面粘着一截字面量——于是解析器很正确地报告：我不知道。文件被读
走了。

---

## 第三层：自己去当 shell

别再当局外的旁观者。把解释器嵌进来，它每要跑一条命令，到你手上的都是
已经成型的参数向量——引号移除、参数展开、命令替换、算术、波浪展开、通
配和 `eval` 统统做完之后的那个。没有语法可以再躲了，因为语法正是刚刚
被吃掉的那部分。

```
命令：    X=.en; eval 'cat ${X}v'
argv：    ["cat", ".env"]
```

```go
interp.New(
    interp.Dir(root),
    interp.StdIO(nil, &stdout, &stderr),
    interp.ExecHandlers(s.execMiddleware),   // 每个程序，最终的 argv
    interp.OpenHandler(s.open),              // 由 shell 自己打开的每个文件
)
```

### 为什么是两个处理器，以及为什么第二个才是意外

```sh
cat < .env
```

这条命令跑的是**不带任何参数**的 `cat`。文件是 shell 打开的，交过去的
是一个文件描述符。检查 argv 的策略从头到尾看不见那个文件名——在*任何*
一层都看不见，包括这一层。只有 `OpenHandler` 能看见它；少了它，基于
argv 的策略上就留着一个洞，形状正好是 `<`。

它有自己单独的测试，因为这恰恰是读者最容易以为"早就覆盖了"的情况。

### 另一样白拿的东西

`sandbox_exec` 对 shell 跑起来的**每一个**程序都会触发，包括那些没人
敲过的：管道、循环、通配和 `eval` 展开出来的那些。这里最大的价值根本
不是拒绝——而是"那条命令到底干了什么"从此变成一个有答案的问题。

```sh
agent --sandbox --observe    # 每次 exec 和 open 都报上来，什么都不拦
```

---

## 诚实的极限

嵌进来的解释器看得见每一条命令。**它看不见一条命令的内部。**

```sh
awk -v a=.en 'BEGIN{f=a"v"; while((getline l < f)>0) print l}'
```

沙箱看到的是 `awk -v a=.en <a program>`，对这段程序的内容没有任何意见
——要有意见就得把 awk 实现一遍。它放行了这次 exec，剩下的活 awk 自己
干完。

这个情况在 `bypass_test.go` 里是**通过的测试**，不是遗漏：

```
--- PASS: TestTheSandboxCannotSeeInsideAProgram
    as documented: the sandbox saw one exec, allowed it, and the program did
    the rest. An embedded interpreter is a policy and observability layer, not
    a security boundary.
```

`perl`、`python`、`ruby`、`node`、`find -exec`、`git -c core.pager=…`、
`make`，以及任何把程序当参数收的东西，全都一样。代码里明确挡掉了
`sh -c`，并在注释里写明这是个推广不了的半吊子办法——枚举解释器就是
黑名单那套游戏，第一层已经输过一次了。

还没完。`isSecretPath` 匹配的是路径的基名：它不解符号链接，不规范化
`..`，也不知道 `/proc/self/cwd/.env` 是同一个文件。就算有个版本把这
三件事都做全了，它照样有竞态，因为路径可以在检查和打开之间被换掉。
**TOCTOU 在沙箱里不是边角情况，它就是标准打法。**

那这一层到底值多少？三样东西，而且都不是可有可无的：

- **可观测性。** 每一次 exec，展开之后的样子，都进了 trace。别的层给
  不了你这个。
- **给合作型 Agent 用的策略。** 模型不是攻击者。它是个系统，偶尔会手
  滑干出有破坏力的事；能接住手滑这一类的层，就算做完了自己该做的事。
- **模型能据此行动的拒绝。** `blocked by the sandbox/exec policy: an
  argument resolves to .env after expansion` 是模型读得懂、能绕开的
  一句话——所以真正起作用的拒绝，就是那句话本身。

至于它不是什么：它不是边界。真正的边界在 shell 底下，在操作系统那一
层——容器、虚拟机、独立 uid、seccomp-bpf、Landlock、mount 命名空间。
这些都要付出配置成本、牺牲可移植性，而且一旦配错，炸开的范围大得多；
没有一个能塞进教学仓库而不把整个仓库变成它自己。**给 coding Agent 的
正确答案几乎永远是"扔进容器里跑"，而本章这一层，是你在容器里面再加
的一层，好让你看得见到底发生了什么。**

---

## 一次真实运行

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

模型读了那句拒绝，读懂了，然后换条路走。整个交互设计就在这里：**只会
说"拒绝"的策略，教会模型再试一次；说清楚自己反对什么的策略，让模型换
个方式把活干完。**

---

## 这个依赖，和它真实的代价

这是唯一引入依赖的阶段，所以值得老老实实记一笔账："加一个依赖"到底
意味着什么。

```
go get mvdan.cc/sh/v3@v3.13.1
  → 声明了 `go 1.25.0`，会把本模块的下限往上抬两个版本

钉到 v3.12.0
  → 声明了 `go 1.23.0`。可以。

……可是 interp 会 import golang.org/x/term
go get golang.org/x/term
  → 升级了 golang.org/x/sys  v0.41.0 → v0.47.0
  → 顶高了本模块             go 1.24.0 → go 1.25.0

把 x/term 钉到 v0.33.0（`go 1.23.0`），x/sys 退回 v0.41.0
  → 下限被拉回 go 1.24.0
```

一次 `go get`，语言版本的下限动了两次——一次是直接依赖抬的，一次是某
个没人挑过的传递依赖抬的。这个仓库的每一位读者本来都得跟着换一套更新
的工具链，而这件事一声不响，不会有任何东西出来提醒你。

> **依赖的 `go` 指令也是它成本的一部分**，而且是你不去看就看不见的那
> 部分。`go get` 之后翻一遍 `go.mod`——纪律就这一条。

让这个依赖值得付的标准是：**它做的事你自己做不了。** JSON 解析器你写
得出来。TOML 解析器你也写得出来，而这个仓库拒绝过两次。可 POSIX shell
的解析器*外加求值器*，带展开、`eval`、算术、通配和作业控制，不是你在
一章里写得出来的东西——而且这一章的论证，缺了它就没有哪个版本站得住
脚，因为论证本身*就是*"你必须成为那个求值器"。

阶段 00–07 仍然只有标准库加 `golang.org/x/sys`，它们编出来的二进制
没有链进这里的任何东西。

---

## 练习

1. **往绕过表里加一条。** 测试会先验证你这条命令真的把文件读出来了才
   算数，所以一条看着聪明、其实跑不通的命令会当场炸给你看。
2. **试着让第二层赢。** 在 `literalWord` 里把反斜杠转义和 `$'…'` 解码
   补上，然后去找下一个绕过。注意你把 shell 重新实现了多大一块，也注意
   你现在维护的是第二个、跟第一个略有出入的 shell——它们的每一处分歧
   都是安全 bug。
3. **把策略翻过来**：不去拒绝某个文件，改成给程序开白名单。然后用
   `find -exec`、`git -c core.pager=` 和 `awk 'BEGIN{system(...)}'`
   穿过去。
4. **用 `--observe` 跑一次真实会话**，在 composer 里读 `sandbox_exec`
   事件。数一数有多少个程序是没人敲过就跑起来的。
5. **加一道根目录围栏**：拒绝工作目录之外的任何 `open`。然后用符号链接
   把它打穿，再用展开之后才出现的 `..` 打穿一次。然后你自己决定：每次
   open 之前都跑一遍 `EvalSymlinks`，这个代价你愿不愿意每开一个文件就
   付一次。
6. **量一量兼容性的代价。** 同一个会话，开 `--sandbox` 和不开各跑一遍，
   找出一条嵌入解释器不支持的命令。它是个非常好的 shell，但它不是你机
   器上那个 shell；到底是哪一处差异会绊住你，值得赶在它要命之前先弄
   明白。
7. **做一个操作系统级的版本。** 一个容器，一条 `podman run --read-only`
   加一个可写挂载，本章这一层放在里面。然后比较这两者各自挡住了什么，
   你会发现它们几乎不相交。

---

## 读完这一章，你能回答什么

**规则为什么是一个指名道姓的文件，而不是"不许干危险的事"？**
因为那么含糊的规则根本测不了，而测不了的策略就是你在瞎猜。一个文件、
一个动词，小到能从头到尾想明白，这也是三套实现能拿来相互比较的唯一
原因。它同时还是本章里最容易的一条规则——而三层做下来，诚实的回答仍然
是：这不是安全边界。

**表里每一条绕过，为什么都得先验证它真的有效？**
因为一条根本没把文件读出来的命令，对"哪个检查漏了它"什么都证明不了，
而这正是这类表最常出错的地方。每一行都先在策略关掉的情况下跑一遍，
确认文件的真实内容出来了，才有资格计数。表是 `bypass_test.go` 生成的，
不是凭记忆写的，所以它跟代码走不散。

**为什么再怎么打磨模式，都救不了字符串检查？**
因为模式读的是源文本，而 shell 读的是源文本代表什么。`cat ".e""nv"`、
`cat .en''v`、`cat .en\v` 和 `cat $'\x2eenv'` 打开的是同一个文件，而
最后那个里面，文件名根本没以文本的形式出现过。你不是在跟攻击者较劲，
你是在跟一门语言较劲。

**真正的 shell 解析器为什么照样输？**
因为它只看得见写下来的东西，而 shell 要跑什么，很大一部分是运行时才定
的。它处理引号是对的，甚至能钻进命令替换、把 `$(echo .env)` 里的 `.env`
找出来——可一个刚刚才赋值的变量、一个参数默认值、一次 `eval`、一个循环
迭代器，对它全是不透明的，十四条命令里有七条穿了过去。求值器输入的
语法树，不告诉你求值器会干什么。

**两个检查都跑，为什么不等于一个更好的检查？**
因为它们漏掉的东西互不相交，不是一个套着另一个：两条只有解析器抓得到，
五条只有正则抓得到。攻击者面对的不是合取式——每条命令只需要一次击败
一个性质，而 shell 的语法正好让它挑着来。`X=.en; eval 'cat ${X}v'` 一次
把两个都干掉，而且只有一行。

**`literalWord` 为什么宁可什么都不返回，也不返回它已经知道的那部分？**
因为半个值会招来错的结论。对 `.en$X` 返回 `".en"`，等于递给调用方一个
能拿去跟策略比对的东西，而比对回来的结果是"安全"。"我不知道"是唯一
一个逼着调用方去面对真实处境的答案。

**基于 argv 的策略，为什么还需要第二个处理器去管文件打开？**
因为 `cat < .env` 跑的是完全不带参数的 `cat`：文件是 shell 自己打开的，
交过去的是一个描述符。再怎么检查 argv 也看不见那个文件名，连语法已经被
吃干净的那一层也看不见，所以少了 `OpenHandler`，策略上就留着一个洞，
形状正好是 `<`。它有自己单独的测试，因为这恰恰是读者最容易以为"早就
覆盖了"的情况。

**`awk -v a=.en '…'` 为什么是一个通过的测试，而不是一个没修的 bug？**
因为沙箱看到的是一次 exec，对参数里那段程序没有任何意见——要有意见就得
把 awk 实现一遍。perl、python、ruby、node、`find -exec`、
`git -c core.pager=…` 和 `make` 全都一样。代码里明确挡掉了 `sh -c`，
并在注释里写明这是个推广不了的半吊子办法，因为枚举解释器就是黑名单那套
游戏，第一层已经输过一次了。

**一层根本不是安全边界的东西，为什么还值得做？**
三样。每一次 exec 展开之后的样子都会进 trace，包括那些没人敲过的程序，
别的层给不了你这个。模型不是攻击者，可它偶尔会手滑干出有破坏力的事，
而一条说清楚自己反对什么的拒绝，能让它换个方式把活干完，而不是原地再
试一次。边界本身属于 shell 底下，属于容器；这一层是你放进容器里面、好
让自己看得见到底发生了什么的那一层。

**加一个依赖，为什么走了四步？**
因为依赖的 `go` 指令也是它成本的一部分。`mvdan.cc/sh/v3` 在 v3.13.1
声明的是 `go 1.25.0`，会把本模块的下限往上抬两个版本；钉到 v3.12.0
解决了这个，可紧接着 `interp` 自己 import 的 `golang.org/x/term` 又把
`x/sys` 从 v0.41.0 拖到 v0.47.0，照样把下限顶高。两个都钉住，才把
`go 1.24.0` 拉了回来。这一路没有任何东西会出声提醒你，所以 `go get`
之后翻一遍 `go.mod`——纪律就这一条。

---

## 思考题

这些题在仓库里没有答案；答案取决于你在造什么。

1. 这里的规则是一个文件。给你自己的 Agent 写一条规则，看它能走多远才
   开始测不动：第二条该写什么？"任何用户会管它叫机密的东西"这一条，
   你打算怎么办？

2. 本章里的模型是合作型的——不是攻击者，只是偶尔有破坏力。可它一旦从
   互联网上读进文本，这句话就不成立了：一次注入就把模型变成了别人的
   通道。本章有哪些部分扛得过这个变化？它会不会改变你往容器里放什么？

3. 沙箱看得见每一个程序，看不见任何一个程序的内部。另一条路是干脆别给
   Agent shell，改给它一批更窄的工具。算一算这在能力上要付多少代价，
   再想想换来的那份策略，你到底写不写得出来。

4. 就算有个检查解了符号链接、规范化了 `..`，还知道
   `/proc/self/cwd/.env` 是同一个文件，它照样会输给任何能在检查和打开
   之间把路径换掉的东西。对于一个你明知道能被打穿的检查，拒绝信息里该
   写什么，文档里又该写什么？把话挑明，会让它更容易被正确地信任，还是
   更不容易？

5. 这个依赖被收下，是因为它做的事你自己做不了；而它现在就坐在策略路径
   上，它里面的 bug 就是策略的 bug。你的第二条标准是什么？如果它的
   维护者只有一个人，而这个人已经很久不出声了，你还收不收？

→ 下一站：[阶段 09：分诊](09-triage.md)——第二部分是从阶段 07 分出去的，
所以沙箱始终是条岔路。

→ 参考：[阶段 01：活下来](01-dont-die.md)、[阶段 07：乘法](07-multiply.md)
