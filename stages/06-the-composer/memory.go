// 阶段 05——记忆，以及上下文允许放在哪里。
//
// "长期记忆"这个词是用来卖向量数据库的。手里有 shell 的 Agent 不需要
// 它，而这个文件就是全部实现：
//
//	记忆就是文件。Agent 用 `cat` 读它，用 `>>` 写它。
//
// 这不是为了教学做的简化，你每天在用的工具就是这么干的。文件可以 grep、
// 可以 diff、可以 review、可以进版本库，还可以被项目的主人用编辑器直
// 接改——这五条向量索引一条都没有，换来的只是在几条笔记上做相似度检
// 索——而那些笔记，`grep` 一下就能找到。
//
// 这个文件更难的那一半不是记忆，是**放置**：给你一段上下文，它该放进
// prompt 的哪里？阶段 04 已经立下规矩：前缀必须逐字节稳定，否则缓存
// 就死。这个文件把那条规矩变成一条分两种情况的规则，而代码长成现在这
// 个样子，就是因为这条规则。
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
// 每一段注入的上下文都只有两种可能，区别不在于它装了什么，而在于**它的
// 值多久变一次**：
//
//	会话内**稳定**  → 放系统提示词，在缓存断点之前。写一次，永远命中
//	                   缓存；不管会话跑多久，这些 token 只花一次钱。
//	                   （记忆文件、cwd、OS、shell、模型上限）
//
//	**易变**        → 在消息创建的那一刻冻进消息里，之后永不重算。
//	                   （时钟、git HEAD、工作树脏不脏）
//
// 第二种是大家会搞错的那一种，而且错的方向是要花钱的那个方向。本能反应
// 是让易变上下文保持*新鲜*——每次请求都重算时间戳，好让模型永远知道现
// 在几点。那就是阶段 04 的 `--break-cache` 实验，量出来是 3.4 倍。
//
// 解法是："新鲜"和"在前缀里"这两件事不可能同时要到；而新鲜恰好是几乎可
// 以白送掉的那一件：用户按下 Enter 时取的快照，对它所属的那个回合是准
// 确的，之后一直不变地留在历史里——这正是"前缀逐字节稳定"的意思。模型
// 每个回合都拿到新鲜信息，**同时**缓存也活着，因为每个回合的快照是一条
// 不同的、永久的行，而不是同一行上换个数。
//
// 一句话，比这个文件其余部分都值钱：**注入一次就冻住；已经在前缀里的东
// 西，永远不要重算。**

// memoryFiles 在启动时按顺序读进来，拼接进系统提示词。两个都是工作目录
// 下的普通 Markdown。
//
// 分家是按作者分的，不是按内容分的：
//
//	AGENTS.md —— 人写给 Agent 的。约定、构建命令、"不要碰 generated/"，
//	               新同事第一天会被告知的那些事。Agent 不该改它。
//	MEMORY.md —— Agent 写给未来的自己的。那些花了工具调用才
//	               查出来的发现。
//
// 分开放着，人就能审 Agent 决定记住了什么，而不用在自己写的指令里翻来
// 翻去，还能拿编辑器把一条坏记忆删掉。Agent 往人的文件里写，早晚会跟
// 它吵起来。
var memoryFiles = []string{"AGENTS.md", "MEMORY.md"}

const memoryFileForWriting = "MEMORY.md"

// loadMemory 把存在的记忆文件读进来，作为一整块返回，再带上找到的文件
// 列表。
//
// 注意它**不做**什么：不监听文件、不每回合重读、也不会察觉 Agent 刚往
// MEMORY.md 追加了一行。这是故意的，是缓存上的决定，不是漏了——记忆
// 待在系统提示词里，会话中途重读它就会改写前缀，把一切都作废。现在写下
// 的笔记，下次会话才生效。用一个回合的延迟换一整个会话的缓存命中，这笔
// 交易站对了边；而知道自己做过这笔交易，是值得的。
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

// remember 往 Agent 的记忆文件里追加一条笔记。
//
// 它是为 `/remember` 命令存在的。Agent 自己就能跑
// `echo ... >> MEMORY.md`，那这里为什么还要有个 Go 函数？值得把话说清
// 楚：它确实能跑，系统提示词里也这么写了。但把记忆完全交给模型自己拿
// 主意，结果就是几乎从不发生：模型不会自发地决定写笔记，因为当前这个
// 回合里没有任何东西为此给它回报。真正能攒下有用记忆的 Agent，都有一
// 个明确的触发器——一条命令、一个会话结束钩子、一句主动来问的
// prompt。机制简单到不值一提，并不能让策略问题跟着消失。
func remember(dir, note string) error {
	path := filepath.Join(dir, memoryFileForWriting)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// 盖日期，因为一条看不出年纪的记忆，你没法决定要不要删。攒半年没有日
	// 期的笔记，就是一份没人修剪、所有人都不再读的文件。
	_, err = fmt.Fprintf(f, "\n- (%s) %s\n", time.Now().Format("2006-01-02"), strings.TrimSpace(note))
	return err
}

// ---------------------------------------------------------------------------
// 稳定上下文：放进系统提示词
// ---------------------------------------------------------------------------

// stableContext 描述的是进程跑着的时候不可能变的东西。
//
// cwd 放在这里、而不是放在易变那一块，是故意的：Agent 的 shell 不是持
// 久的（每条命令都是新进程，阶段 00），所以命令里的 `cd` 挪不动它。唯
// 一会让这个判断失效的事情，是给 Agent 一个持久 shell——到那时候 cwd
// 就变成易变的，必须搬到另一块去。值得留意的是：执行模型上的一个改动，
// 会一路传导到缓存的布局上。
func stableContext(shell, cwd string) string {
	return fmt.Sprintf(`<environment>
os: %s/%s
shell: %s
working directory: %s
</environment>`, runtime.GOOS, runtime.GOARCH, shell, cwd)
}

// ---------------------------------------------------------------------------
// 易变上下文：一次冻进一条消息里
// ---------------------------------------------------------------------------

// volatileContext 给会动的那些东西拍一张快照。
//
// 它要跑 git，这要花掉一个进程。每个用户回合花一次担得起，每次请求花一
// 次就担不起了——这是快照挂在用户消息上、而不是在请求时重建的另一个理
// 由。这里最省的设计和缓存上最正确的设计，恰好是同一个设计；这通常说明
// 边界划在了对的地方。
func volatileContext(shell string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<now>%s</now>\n", now.Format("2006-01-02 15:04:05 -0700"))

	// 一条命令，一个进程，要的东西全拿到。`|| true` 是有讲究的：它会在不是
	// 仓库的目录里跑；而上下文探针要是把失败当成内容报上去，就等于告诉模
	// 型：它的环境是坏的。
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

// userTurn 为人类敲进来的一句话构造消息，并把易变快照冻在它旁边。
//
// 是两个 block，而不是拼成一个字符串，因为阶段 06 渲染它们的方式不一
// 样：上帝视角显示注入进去的原样，模型视角显示模型收到的那条消息。在
// 这里合并，那个区分就再也找不回来了——而"模型到底看到了什么"这个问
// 题，只有在你从没把答案扔掉的情况下才答得上来。
func userTurn(text, volatile string) Msg {
	m := Msg{Role: RoleUser}
	if volatile != "" {
		m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: volatile + "\n\n"})
	}
	m.Blocks = append(m.Blocks, Block{Kind: BlockText, Text: text})
	return m
}
