// 阶段 07——子 Agent，就是把同一个循环再调一次。
//
// 这里没有框架，也没有编排层。子 Agent 就是：
//
//	一个全新的 []Msg、另一份系统提示词、同一个 provider、同一套
//	工具——而且它返给调用方的是**文本**，不是它的对话记录。
//
// 最后那半句才是全部的产品。子 Agent 做的每一件事——每次工具调用、每
// 一份 40kB 的命令输出、每一步走错又退回来的弯路——都发生在一个跑完
// 就扔掉的消息数组里。父 Agent 的上下文只按报告的长度增长，别的一点
// 不涨。
//
// 所以有件事得说清楚，因为它和大家的想当然正好相反：
//
//	**子 Agent 不省 token，它省的是"上下文"。**
//
// 它的总 token 通常比内联做完这件事*更多*——子 Agent 要重读一遍系统
// 提示词，重新确立自己在做什么，还要重新发现父 Agent 早就知道的东
// 西。它买到的是父 Agent 的窗口不会被填满，而那才是真正会耗尽的资
// 源。阶段 05 量过被填满之后会发生什么。
//
// 第二件值得注意的事，是有哪些东西**不是**新的。父 Agent 本来就有循
// 环，子 Agent 就是那个循环；父 Agent 本来就有总线，子 Agent 把它
// fork 一下；父 Agent 本来就有 compactor 和权限闸，子 Agent 跟它共用。这
// 个文件里大约一百行是新功能，而其中大多数是保险丝。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 子 Agent 的系统提示词。
//
// 要紧的是第三段，它把机制讲给模型听，而不是瞒着模型。不知道自己的
// 对话记录会被扔掉的子 Agent，写出来的是过程小结（"我看了几个文件，
// 发现了一些东西"），因为聊天的一个回合通常就长这样。明明白白告诉
// 它，只有最后那条消息能活下来，它写出来的就是报告。
const subagentSystem = `You are a subagent. Another agent has delegated one task to you and is waiting.

You have the same shell it has, and the same working directory. Do the task.

Everything you do here is discarded when you finish EXCEPT your final message.
Your caller will never see your commands, your reasoning, or your tool output —
only the last thing you say. So your final message has to stand alone:

- Give exact paths, exact command lines, exact identifiers, exact error text.
  Your caller cannot re-run anything to check, and cannot ask you a follow-up.
- State what you could NOT determine, and why. A gap you name is useful; a gap
  you paper over sends your caller down a wrong path with confidence.
- Report findings, not process. "src/auth/token.go:88 hardcodes a 3600s TTL" is
  worth a line; "I searched the repo and looked at several files" is worth none.

You cannot ask the user anything: there is no user here. If the task is
ambiguous, choose the most useful reading, do it, and say which reading you
chose.`

func taskToolDef() Tool {
	return Tool{
		Name: "task",
		// 这段描述是冲着*经济账*写的，因为模型要做的正是这个决定。
		// 只说工具做什么的描述，等于没告诉模型什么时候该伸手去拿。
		Description: "Delegate a self-contained piece of work to a subagent with its own context window. " +
			"The subagent has the same shell and returns only a final written report; its commands and " +
			"output never enter your context. Use this for work that will read a lot and conclude a little — " +
			"searching a large codebase, investigating a failure, surveying files. Do not use it for a single " +
			"command, and do not use it for work whose intermediate output you need to see.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A three-to-six word label for this task, shown to the user.",
				},
				"prompt": map[string]any{
					"type": "string",
					"description": "The full task. The subagent shares your working directory but none of " +
						"your conversation, so state everything it needs: what to look for, what you already " +
						"know, and exactly what to report back.",
				},
			},
			"required":             []string{"description", "prompt"},
			"additionalProperties": false,
		},
	}
}

// parseTaskArgs 照着 parseBashArgs 抄，指针字段也一并抄过来。
//
// 阶段 01 是吃过亏学到的：值类型的 string 字段会让 json.Unmarshal 在
// 一份根本没带这个键的 payload 上成功，于是截断的工具调用变成空任
// 务，而不是错误。这里有两个必填字段，所以两个指针。
func parseTaskArgs(raw string) (description, prompt string, err error) {
	var args struct {
		Description *string `json:"description"`
		Prompt      *string `json:"prompt"`
	}
	if e := json.Unmarshal([]byte(raw), &args); e != nil {
		return "", "", fmt.Errorf("tool call arguments were not valid JSON: %v", e)
	}
	if args.Prompt == nil || strings.TrimSpace(*args.Prompt) == "" {
		return "", "", fmt.Errorf(`the "prompt" field was missing or empty; a subagent with no task cannot do anything`)
	}
	d := "subtask"
	if args.Description != nil && strings.TrimSpace(*args.Description) != "" {
		d = strings.TrimSpace(*args.Description)
	}
	return d, *args.Prompt, nil
}

// ---------------------------------------------------------------------------
// 派生
// ---------------------------------------------------------------------------

// spawn 把一个子 Agent 跑到结束，返回它的报告。
//
// 注意什么共享、什么不共享。共享的：provider、HTTP 客户端、权限闸、shell
// 配置、沙箱，还有总线的 core——这样子 Agent 的权限提问会送到同一个人面
// 前，它的事件也落进同一份有序的 trace。不共享的：消息数组、系统提示词、
// compactor 和回合预算。这道分界线恰好就是"父 Agent 不能丢的状态"对上"子
// Agent 不能继承的状态"。
func (a *agent) spawn(callID, description, prompt string) (string, Usage, error) {
	started := time.Now()
	agentID := fmt.Sprintf("%s#%d", description, a.nextChild())

	a.bus.Emit(Event{
		Kind: KindSubagentStart, ToolID: callID,
		ToolName: description, Text: prompt,
	})

	child := a.newChild(agentID, func() string { return subagentSystem + para + a.stable })

	msgs := []Msg{TextMsg(RoleUser, prompt)}
	msgs = child.runTurn(msgs)

	report := lastAssistantText(msgs)
	if strings.TrimSpace(report) == "" {
		// 什么都不返回的子 Agent 比出错更糟，因为父 Agent 会把空串
		// 当成结论。把话挑明说出来。
		report = "[the subagent produced no final report — it may have hit its turn limit or been cut off. Treat this as a failure, not as an empty result.]"
	}

	a.bus.Emit(Event{
		Kind: KindSubagentEnd, ToolID: callID, ToolName: description,
		Text: report, Bytes: len(report), Millis: time.Since(started).Milliseconds(),
		Usage: &child.spent, Turn: child.turnsUsed,
	})
	return report, child.spent, nil
}

func (a *agent) nextChild() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.children++
	return a.children
}

// newChild 造出的子 Agent，该共享的都共享，不该继承的一样不继承。
//
// 共享的：provider、HTTP 客户端、权限闸、shell 配置、沙箱，还有总线的
// core——这样子 Agent 的权限提问会送到同一个人面前，它的事件也落进同一份
// 有序的 trace。不共享的：消息数组、系统提示词、compactor 和回合预算。
//
// 沙箱值得单独说一句，因为它正是那个缺了也没有任何症状的条目。它一身两
// 职：既是一条命令要守的策略，也是这条策略看见过什么的那份记录。给子
// Agent 配一个它自己的沙箱，就得给它一个根目录，而唯一站得住的根目录就是
// 父 Agent 那个——于是这份拷贝会是"父 Agent 的策略，配一份私有的审计记
// 录"，而子 Agent 产生的每一次 exec、每一次 open、每一次拒绝，都会从
// main 在会话结束时打出的那份 report() 里、以及 /status 在会话进行中显示
// 的那份里，一并消失。一个沙箱在并发的子 Agent 之间共用是安全的，因为
// run 每次调用都新建一个解释器，而这个结构体上所有可变的东西——那几个审计
// 切片，还有 /open 会挪的 root——都在它那把锁后面。总线根本不在上面，理由
// 写在那边：这条总线是谁的，每次调用都不一样。
//
// 一个字段一个字段写出来，而不是写 `child := *a`——后者更短，而且 `go
// vet` 拒得没错：agent 里有 sync.Mutex，复制含互斥锁的结构体，副本拿到的
// 锁就停在原件当时的那个状态上。写全了也更诚实：这里每一行，都是在决定子
// Agent 到底是什么。它的代价是一项长期的义务：这个函数写完之后才加到
// agent 上的字段，在有人回到这里补一笔之前，子 Agent 就是拿不到，而工具
// 链里没有任何东西会提醒这一句。
func (a *agent) newChild(agentID string, system func() string) *agent {
	child := &agent{
		p: a.p, httpc: a.httpc, g: a.g, cfg: a.cfg, sb: a.sb,
		bus:       a.bus.Fork(agentID),
		memoryDir: a.memoryDir,
		stable:    a.stable,
		depth:     a.depth + 1,
		maxDepth:  a.maxDepth,
		system:    system,

		// compactor 要新的，因为子 Agent 的对话是另一场对话。共用一个，
		// 子 Agent 的估算器就是拿父 Agent 的流量校准的——通常也够接近
		// 了，而"通常够接近"正是共享可变对象在半年后变成 bug 的路子。
		comp: newCompactor(a.comp.window, a.comp.threshold, a.comp.keepRatio),
	}
	child.comp.est.ratio = a.comp.est.ratio // 白送一次提示，之后它自己校准

	// 子 Agent 自己的回合预算，默认比父 Agent 小：要跑三十轮才够的子
	// Agent，接到的任务本该拆成三个子 Agent，而唯一会告诉你这件事的，就
	// 是这道保险丝。
	child.cfg.maxTurns = a.cfg.subTurns
	return child
}

// lastAssistantText 就是子 Agent 的返回值：它最后说的那句话。
func lastAssistantText(msgs []Msg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			if t := msgs[i].Text(); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 工具表，它是深度的函数
// ---------------------------------------------------------------------------

// tools 返回这个 agent 可以调用的东西。
//
// 到了深度上限，`task` 工具是被**摘掉**，不是被拒绝。这个区别是有意
// 为之，值得单占一段：
//
// 运行时拒绝要花一整趟往返——模型写出工具调用，宿主把它拒掉，模型读
// 到拒绝再换个别的试——而且它还要在每一次永远用不上这个工具的请求里，
// 付掉工具定义的 token。更糟的是，这条规矩模型看得出是随意的，而模型
// 对付随意的规矩，办法就是换个说法。
//
// 不在列表里的工具不是规矩。没什么可争的，也没什么可绕的，模型就在
// 手上这些工具里做计划，而这正是你想要的。
func (a *agent) tools() []Tool {
	if a.depth >= a.maxDepth {
		return []Tool{bashToolDef()}
	}
	return []Tool{bashToolDef(), taskToolDef()}
}

// ---------------------------------------------------------------------------
// 跑完一个回合的工具调用，其中有些同时跑
// ---------------------------------------------------------------------------

// dispatch 执行一个 assistant 回合里的每一次工具调用，并且**按模型发
// 出的顺序**返回结果。
//
// 子 Agent 的调用并发跑，其余的按顺序跑。要留意的是那条顺序保证：执
// 行是并发的，历史是确定的。结果要是按完成先后追加，同一次会话重放
// 两遍会产出两份不同的消息数组、两份不同的 prompt 前缀，以及——照阶
// 段 04 的说法——永远命不中的缓存。并发可以改变事情花多长时间，不可
// 以改变对话说了什么。
func (a *agent) dispatch(turn int, calls []Block) ([]Block, bool) {
	results := make([]Block, len(calls))
	texts := make([]string, len(calls))
	stopped := false

	// 第 1 趟：串行的活儿，以及所有调用的权限裁决。
	//
	// 每一个权限问题都在**这里**问，在同一个 goroutine 上，在并发开始之
	// 前。权限提问要是被两个 goroutine 同时写出来，一行上就会出现两个半
	// 截的问题，然后读一个答案当两个用——这是穿着 UI bug 外衣的安全
	// bug。
	type pending struct {
		i           int
		description string
		prompt      string
	}
	var async []pending

	for i, c := range calls {
		if stopped {
			texts[i] = "[not executed: the session was stopped.]"
			continue
		}
		switch c.Name {
		case "task":
			description, prompt, err := parseTaskArgs(c.Args)
			if err != nil {
				texts[i] = fmt.Sprintf("[%v]", err)
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name,
				Command: description + ": " + firstLine(prompt)})
			v, why := a.g.ask("subagent — " + description)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this subagent. Do not retry it unchanged.]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session.]"
			default:
				async = append(async, pending{i, description, prompt})
			}

		case "bash":
			command, err := parseBashArgs(c.Args)
			if err != nil {
				texts[i] = fmt.Sprintf("[%v]", err)
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})
			v, why := a.g.ask(command)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this command. Do not retry it unchanged.]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session.]"
			default:
				texts[i] = a.runCommand(turn, c.ID, command)
			}

		default:
			// 模型编出来的工具名。这种事会发生，而对策是把话说准，而
			// 不是让整个回合失败：模型能从"没有这个工具"里恢复过来，
			// 从丢掉的结果里恢复不了。
			texts[i] = fmt.Sprintf("[there is no tool called %q. The tools available to you are listed in this request.]", c.Name)
		}
	}

	// 第 2 趟：子 Agent，一起上。
	if len(async) > 0 {
		var wg sync.WaitGroup
		for _, p := range async {
			wg.Add(1)
			go func(p pending) {
				defer wg.Done()
				report, _, err := a.spawn(calls[p.i].ID, p.description, p.prompt)
				if err != nil {
					texts[p.i] = fmt.Sprintf("[the subagent failed: %v]", err)
					return
				}
				texts[p.i] = report
			}(p)
		}
		wg.Wait()
	}

	for i, c := range calls {
		results[i] = a.emitResult(turn, c.ID, texts[i])
	}
	return results, stopped
}

// runCommand 是 dispatch 的 bash 那一半，和阶段 06 相比没有变化，只是
// 它把渲染好的结果返回，而不是追加进去。
func (a *agent) runCommand(turn int, callID, command string) string {
	a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})

	// 阶段 08：同一条命令，改由我们自己拿着的解释器来跑，而不是由我们只管
	// 起一下的 bash 来跑。下游的一切——截断、事件、告诉模型什么——完全没
	// 变，因为从阶段 01 起，exec.go 就一直把"跑命令"和"渲染结果"当两件事在
	// 做。
	var r execResult
	if a.sb != nil {
		r = a.sb.run(command, a.cfg.timeout, a.bus)
	} else {
		r = runBash(a.cfg.shell, command, a.cfg.timeout)
	}
	rendered, truncated := r.render(a.cfg.maxOutput)
	a.bus.Emit(Event{
		Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
		ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
		Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
	})
	return rendered
}

// 用户批准子 Agent 时读到的就是 firstLine，所以它有两件活儿：显示点
// 东西出来，以及在内容不完整时绝不读起来像完整的。
//
// 前导空白是在切之**前**去掉的，不是切完再去。以换行开头的 prompt——
// 模型写多段任务时大多如此——从前会产出字符串 " …"：省略号前面什么都
// 没有，而这一行正是要人来授权的那行。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
