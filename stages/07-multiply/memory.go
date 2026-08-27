// 阶段 05 — 记忆，以及上下文可以去的地方。
//
// "长期记忆"是推销向量数据库的说辞。
// 带 shell 的 Agent 不需要它，这个
// 文件就是完整的实现：
//
//	记忆是文件。Agent 用 `cat` 读它，
//	用 `>>` 写它。
//
// 这不是为了教学而做的简化；这就是
// 你每天在用的工具本来就会做的事。
// 文件是可以 grep 的、可以 diff 的、
// 可以审查的、可以版本化的，也能被
// 这个文件所描述项目的那个人编辑——
// 这五个特性，嵌入索引一个都没有，
// 它换来的只是对笔记做相似度搜索，
// 而这些笔记，`grep` 本来就能找到。
//
// 这个文件较难的那一半不是记忆，
// 而是**放置**：给定一块上下文，
// 它该放进 prompt 的哪个位置？
// 阶段 04 确立了一条规则：前缀
// 必须字节稳定，否则缓存就会失效。
// 这个文件把它变成一条分两种情况
// 讨论的规则，这条规则，就是代码
// 会长成现在这个样子的原因。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 放置规则
// ---------------------------------------------------------------------------
//
// 每块被注入的上下文，都是两种
// 东西之一，区别不在于它包含
// 什么，而在于**它的值多久变
// 一次**：
//
//	会话的稳定   → 系统提示词，在缓存断点之前。
//	                  写一次，永远缓存，只花费其 token
//	                  恰好一次，无论会话运行多长。
//	                  （记忆文件，cwd，OS，shell，模型限制）
//
//	易变                 → 在消息创建的那一刻冻结进消息，
//	                  此后永不重新计算。
//	                  （时钟，git HEAD，工作树脏度）
//
// 第二种情况是人们常常搞错的，
// 而且错的方向总是要花钱的那个
// 方向。本能反应是让易变上下文
// 保持**新鲜**——每个请求都重新
// 计算一次时间戳，好让模型始终
// 知道现在几点。那就是阶段 04 的
// `--break-cache` 实验，测出的
// 结果是 3.4 倍。
//
// 解法是："新鲜"和"在前缀里"，
// 这两样你不能同时拥有，而新鲜，
// 恰恰是那个你几乎可以免费放弃
// 的东西：用户按下 Enter 时取得
// 的快照，在它所属的整个回合内
// 都是准确的，此后在历史里也
// 保持不变——这正是字节稳定
// 前缀的意思。模型每个回合都能
// 拿到新鲜信息，**且**缓存幸存，
// 因为每个回合的快照都是一条
// 不同的、永久的行，而不是同一
// 行里塞进一个不断变化的值。
//
// 一句话，比这个文件其余部分
// 加起来都更有价值：**注入一次，
// 然后冻结；前缀里已经有的东西，
// 永不重新计算。**

// memoryFiles 在启动时按顺序读取，
// 并拼接进系统提示词。两者都是
// 工作目录中的纯 Markdown。
//
// 分割是按作者，不是按内容：
//
//	AGENTS.md — 由人类为 Agent 写。惯例，
//	             构建命令，"不要接触 generated/"，
//	             一个新同事会在第一天被告知的东西。
//	             Agent 不应编辑它。
//	MEMORY.md — 由 Agent 为未来的自己写。
//	             那些得靠工具调用才能重新
//	             发现的东西。
//
// 保持它们分开，意味着人类可以
// 审查 Agent 决定记住了什么，
// 而不必把自己写的指令也翻一遍，
// 还可以用编辑器删掉一条错误的
// 记忆。如果 Agent 也往人类的文件
// 里写东西，迟早会跟人类的意见
// 对不上。
var memoryFiles = []string{"AGENTS.md", "MEMORY.md"}

const memoryFileForWriting = "MEMORY.md"

// loadMemory 读取所有存在的记忆
// 文件，把它们作为一整块返回，
// 再加上找到的文件列表。
//
// 注意它**不**做什么：不监视文件，
// 不在每个回合重新读取它们，也
// 不会注意到 Agent 刚刚往 MEMORY.md
// 里追加了内容。这是故意的，是
// 一个缓存上的决定，不是疏忽——
// 记忆坐在系统提示词里，会话
// 中途重读它会重写前缀，让一切
// 缓存失效。现在写下的笔记，
// 要到下一次会话才会生效。用
// 一回合的延迟，换一整个会话的
// 缓存命中，这笔交易划算，而且
// 值得你清楚意识到自己做了这个
// 选择。
func loadMemory(dir string, bus *Bus) (string, []string) {
	var b strings.Builder
	var found []string
	for _, name := range memoryFiles {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		found = append(found, name)
		fmt.Fprintf(&b, "<memory file=%q>\n%s\n</memory>\n\n", name, strings.TrimSpace(string(raw)))
		if bus != nil {
			bus.Emit(Event{Kind: KindMemoryLoaded, Path: path, Bytes: len(raw)})
		}
	}
	return b.String(), found
}

// remember 把一条笔记追加进
// Agent 的记忆文件。
//
// 这是为 `/remember` 命令而存在的，
// Agent 明明可以自己运行
// `echo ... >> MEMORY.md`，这里却
// 还专门写了一个 Go 函数，这一点
// 值得说清楚为什么。它确实可以，
// 系统提示词里也是这么告诉它的。
// 但把记忆完全交给模型自行决定，
// 就意味着它几乎从不会发生：
// 模型不会自发决定写笔记，因为
// 当前回合里没有任何东西会奖励
// 这么做。每个真正积累起有用
// 记忆的 Agent，都有一个显式的
// 触发点——一条命令、一个会话
// 结束钩子、一句会主动问的
// prompt。机制本身简单得不值
// 一提，但这并不会让策略问题
// 跟着消失。
func remember(dir, note string) error {
	path := filepath.Join(dir, memoryFileForWriting)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// 打上日期戳，因为一条你判断不出
	// 年龄的记忆，就是一条你没法
	// 决定删不删的记忆。六个月不带
	// 日期的笔记，攒成的是一个没人
	// 修剪、所有人都不再读的文件。
	_, err = fmt.Fprintf(f, "\n- (%s) %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(note))
	return err
}

// ---------------------------------------------------------------------------
// 稳定上下文：进入系统提示词
// ---------------------------------------------------------------------------

// stableContext 描述的是进程运行
// 期间不会改变的东西。
//
// cwd 之所以在这里，而不是在
// 易变块里，是故意安排的：Agent
// 的 shell 不是持久的（每个命令
// 都是一个新进程，阶段 00），
// 所以命令内部的 `cd` 无法真正
// 移动它。唯一会让这个假设变错
// 的，是给 Agent 一个持久 shell——
// 到那时 cwd 就变成了易变的，
// 必须挪到另一块里去。值得
// 留意执行模型上的一个改动，
// 是如何直接传导进缓存布局的。
func stableContext(shell, cwd string) string {
	return fmt.Sprintf(`<environment>
os: %s/%s
shell: %s
working directory: %s
</environment>`, runtime.GOOS, runtime.GOARCH, shell, cwd)
}

// ---------------------------------------------------------------------------
// 易变上下文：冻结到一条消息，一次
// ---------------------------------------------------------------------------

// volatileContext 取的是那些会
// 变化的东西的快照。
//
// 它会运行 git，这要花掉一个
// 进程的开销。这份开销，每个
// 用户回合负担得起，但换成每个
// 请求都要负担，就不行了——这
// 也是快照被附加在用户消息上，
// 而不是在请求时重新构建的另
// 一个原因。便宜的设计和对缓存
// 友好的设计，在这里恰好是同
// 一个设计，这通常就是边界
// 划对了地方的迹象。
func volatileContext(shell string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<now>%s</now>\n", now.Format("2006-01-02 15:04:05 -0700"))

	// 一个命令，一个进程，所有我们
	// 想要的东西。`|| true` 很重要：
	// 这段命令会在不是 git 仓库的
	// 目录里运行，而且，如果这个
	// 上下文探针把失败当成内容原样
	// 报告出来，就等于在告诉模型：
	// 你的环境坏了。
	const gitProbe = `git rev-parse --abbrev-ref HEAD 2>/dev/null && ` +
		`git status --porcelain 2>/dev/null | wc -l && ` +
		`git log -1 --format=%s 2>/dev/null || true`
	r := runBash(shell, gitProbe, 3*time.Second)
	lines := strings.Split(strings.TrimSpace(r.Stdout), "\n")
	if r.ExitCode == 0 && len(lines) >= 2 && lines[0] != "" {
		subject := ""
		if len(lines) >= 3 {
			subject = strings.TrimSpace(lines[2])
		}
		fmt.Fprintf(&b, "<git branch=%q dirty=%q>%s</git>\n",
			strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), subject)
	}
	return strings.TrimSpace(b.String())
}

// userTurn 为人类输入的某一样
// 东西构建消息，同时把易变
// 快照冻结在它旁边。
//
// 两块，而不是拼成一个字符串，
// 是因为阶段 06 会用不同方式
// 呈现它们：上帝视角显示的是
// 究竟注入了什么，模型视角
// 显示的则是模型实际收到的
// 那条消息。要是在这里把两者
// 合并，这个区别就再也恢复
// 不了——而"模型实际看到了
// 什么"这个问题，只有在你从来
// 没有把答案扔掉的前提下，
// 才回答得出来。
func userTurn(text, volatile string) Msg {
	m := Msg{Role: RoleUser}
	if volatile != "" {
		m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: volatile + "\n\n"})
	}
	m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: text})
	return m
}
