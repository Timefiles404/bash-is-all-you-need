---
name: new-stage
description: 给这个课程增加一个阶段 —— 快照规则、章节形状、还有一个章节要测量什么
---

# 增加一个阶段

每个 `stages/NN-name/` 是 Agent 在课程一个点的**完整、独立的快照**。读者应该能做 `diff stages/NN stages/NN+1` 然后看到恰好一个想法降临。

## 不可协商的规则

- **复制前一个阶段，不要导入它。** 快照之间的重复是这个特性。永远不要把共享代码重构进一个通用包。
- **没有依赖**，标准库和 `golang.org/x/sys` 除外。第 08 阶段是唯一的例外，它的章节基本上是关于为什么那一个配得上自己位置的论证。
- **每个阶段一个想法。** 如果 diff 引入两个，那就是两个阶段。

## 这一章

`docs/NN-name.md`，它需要所有这些：

- 前三段里的想法，包括它花什么。
- 一个**"来自一个真实的运行"**部分，有你实际捕捉的输出。发明的例子破坏了整个仓库的前提。
- 至少一个**测量**，说清楚比较的对象，并点出混淆因素。如果测量破坏了章节的论点，说这样 —— 第 04 阶段发现没有缓存标记比短会话上有标记更好，第 05 阶段发现上下文压缩花的比省的多。两个都直说了。
- 至少一个**在写它时发现的失败**。这些是仓库里最有价值的段落。寻找它们而不是写一个干净的叙述。
- 故意弄坏点什么的练习。

## 提交前验证

```sh
gofmt -l stages/                        # empty
go vet ./...                            # clean
go test -race ./...                     # green
GOOS=linux go build ./stages/...        # the platform files are real
GOOS=darwin go build ./stages/...
grep -rnE 'sk-[A-Za-z0-9]{20,}' --exclude-dir=.git .   # no keys, ever
```

只在 `sandbox/` 里运行 Agent —— 它执行模型说的话。

## 注释

匹配 `stages/04-the-cache/render.go` 的密度。注释解释**为什么**并命名它们阻止的失败；它们从不重申代码。
