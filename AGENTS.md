# AGENTS.md

这个仓库的约定。第 05 阶段的 Agent 在启动时读这个文件，把它放在系统提示词里，所以这既是文档又是这个特性的一个现场演示 —— 见 [docs/05-live-forever.md](docs/05-live-forever.md)。

## 这个仓库是什么

一个渐进式课程。`stages/` 下的每个目录都是 Agent 在课程某个点的**完整、独立的快照**。它们之间的重复是刻意的：读者应该能做 `diff stages/04-the-cache stages/05-live-forever` 然后看到恰好一个想法降临。

**不要把共享代码重构进一个通用包。** 那是唯一会破坏这个仓库存在意义的改变。

课程分两段。**第一阶段（00–08）是仪表盘，第二阶段（09 起）是生产环境里的失败。**
第二阶段的每个新阶段都从**第 07 阶段**复制，不是从上一个编号复制：第 08 阶段是整个
仓库唯一带依赖的地方，而且明说是可选的，一路带下去就等于让它变成必需。加新阶段的时候
`cp -r stages/07-multiply stages/NN-name`，然后只加那一个想法。

## 规则

- **没有依赖**，标准库和 `golang.org/x/sys` 除外，后者钉在 v0.41.0 因为 v0.42+ 声明了 `go 1.25.0`。没有 SDK、没有 TUI 框架、没有 JSON 库、没有测试框架。
  第 08 阶段是唯一的例外（`mvdan.cc/sh/v3`，钉在 v3.12.0，`golang.org/x/term` 在 v0.33.0 来保持模块底线在 go 1.24.0）。在加任何东西之前，读一下新的 `go.mod` —— 一个依赖的 `go` 指令是它成本的一部分，也是没有东西会公告的那部分。
- **每个教学声明都建立在 `docs/wire-notes.md` 上**，它记录一个真实网关实际发出的东西，逐字节。在协议文档和观测到的行为不一致的地方，观测赢了，不一致也会被记录下来。
- **注释解释*为什么*，并命名它们阻止的失败。** 它们从不重申代码。匹配 `stages/04-the-cache/render.go` 的密度。
- **一个章节报告它测量到的东西**，包括测量破坏了章节论点的时候。第 04 阶段发现没有缓存标记比短会话上有标记更好；第 05 阶段发现上下文压缩花的比省的多。双方都直说了。
- **测试只有在变异测试以后才接受。** 故意破坏代码，一次一个改变，然后确认一个测试对每个改变都失败。一个存活的变异意味着一个丢失的测试。一个**编译**失败的变异不能证明任何东西 —— 在相信一次"被捕捉"之前，先确认测试二进制真的编译通过了。

## 命令

```sh
go build ./stages/06-the-composer      # or any stage
go test ./...                          # all stages
gofmt -l stages/                       # must be empty
go vet ./...                           # must be clean
GOOS=linux go build ./stages/06-the-composer    # the platform files are real
GOOS=darwin go build ./stages/06-the-composer
```

## 运行 Agent

它执行模型说的话。用 `sandbox/`（gitignored）作为工作目录，永远不要用仓库根。凭证来自 `.env`，它被 gitignore 了从不提交：

```sh
set -a && . ./.env && set +a
```
