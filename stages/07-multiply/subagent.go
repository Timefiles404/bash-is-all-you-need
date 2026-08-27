// 阶段 07 —— 子 Agent，即同一个主循环被再调用一次。
//
// 这里没有框架也没有编排层。子 Agent 就是：
//
//	一个新的 []Msg，一个不同的系统提示词，同一个供应商，同一套工具
//	—— 它返回**文本**给调用者，不是它的对话历史。
//
// 最后这一条是整个产品。子 Agent 做的一切 —— 每个工具调用、每个 40kB 的
// 命令输出、它走进又退出来的每一次弯路 —— 都发生在一个结束时被扔掉的消
// 息数组中。父 Agent 的上下文只会增长报告的长度，别的都不增长。
//
// 所以要讲清楚的是，因为这正好是人们假设的反面：
//
//	**子 Agent 不省 token。它省的是上下文。**
//
// 它的总 token 通常比内联做完这件事**更多** —— 子 Agent 要重读一遍系统提
// 示词，重新确立自己在做什么，还要重新发现父 Agent 早就知道的东西。它买
// 到的是父 Agent 的窗口不会被填满，而那才是真正会耗尽的资源。阶段 05 测
// 过它填满后会发生什么。
//
// 第二个值得注意的是什么**不是**新的。父 Agent 已经有一个主循环；子
// Agent 就是那个主循环。父 Agent 已经有一条事件总线；子 Agent fork 了它。
// 父 Agent 已经有一个压缩器和一个权限闸；子 Agent 共享它们。这个文件的大
// 约一百行是这个功能，其中大部分是保险丝。
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
// 第三段才是重点，它是向模型解释的机制，而不是隐藏起来的。一个不知道自己
// 的对话历史被丢弃的子 Agent 会写一个自己过程的总结（"我看了几个文件找到
// 了一些东西"），因为那是一个聊天回合通常的样子。明确告诉它最后一条消息
// 是唯一会存活的东西，它就会写一个报告。
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
		// 描述是为了**经济学**而写的，因为那是模型必须做出的决定。一个只说
		// 明工具做什么的工具描述，没法告诉模型该在什么时候用它。
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

// parseTaskArgs 镜像 parseBashArgs，包括指针字段。
//
// 阶段 01 是吃过亏才明白这一点的：一个值类型的字符串字段会让
// json.Unmarshal 在根本没有这个 key 的 payload 上成功，所以一个被截断
// 的工具调用变成了空任务而不是错误。这里两个必需字段，所以两个指针。
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
// 生成子 Agent
// ---------------------------------------------------------------------------

// spawn 运行一个子 Agent 到完成并返回它的报告。
//
// 注意什么被共享什么不被共享。共享的：供应商、HTTP 客户端、权限闸、shell
// 配置和事件总线核心 —— 所以子 Agent 的权限提示到达同一个人，子 Agent 的
// 事件进入同一条有序的 trace。不共享的：消息数组、系统提示词、压缩器和回
// 合预算。这条分割线，正好是"父 Agent 必须不失去的状态"与"子 Agent 必
// 须不继承的状态"之间的界线。
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
		// 一个返回空的子 Agent 比错误还糟糕，因为父 Agent 会把空字符串当作一个发
		// 现。把话讲清楚。
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

// newChild 构建一个子 Agent，它共享必须共享的，不继承必须不继承的。
//
// 共享的：供应商、HTTP 客户端、权限闸、shell 配置和事件总线核心 —— 所以
// 子 Agent 的权限提示到达同一个人，它的事件进入同一条有序的 trace。不共
// 享的：消息数组、系统提示词、压缩器和回合预算。
//
// 它是逐字段写出来的，而不是作为 `child := *a`，这样更短而且 `go vet` 正
// 确地拒绝它：agent 拥有一个 sync.Mutex，复制一个包含它的结构体会给副本
// 一个已经处于原件状态的互斥锁。这种明确的形式也是诚实的形式 —— 它的每一
// 行都是关于什么是子 Agent 的一个决定。
func (a *agent) newChild(agentID string, system func() string) *agent {
	child := &agent{
		p: a.p, httpc: a.httpc, g: a.g, cfg: a.cfg,
		bus:       a.bus.Fork(agentID),
		memoryDir: a.memoryDir,
		stable:    a.stable,
		depth:     a.depth + 1,
		maxDepth:  a.maxDepth,
		system:    system,

		// 一个新的压缩器，因为子 Agent 的对话是一段不同的对话。共享一个会意味着
		// 子 Agent 的估算器是在父 Agent 的流量上校准的 —— 通常足够接近，而"通常
		// 足够接近"就是一个共享的可变对象在六个月后如何变成 bug 的。
		comp: newCompactor(a.comp.window, a.comp.threshold, a.comp.keepRatio),
	}
	child.comp.est.ratio = a.comp.est.ratio // 一个免费提示，然后它校准

	// 子 Agent 自己的回合预算，默认比父 Agent 的要小：一个需要三十轮的子
	// Agent，接到的是一个本该拆成三个子 Agent 来做的任务，保险丝是唯一会
	// 告诉你这一点的东西。
	child.cfg.maxTurns = a.cfg.subTurns
	return child
}

// lastAssistantText 是子 Agent 的返回值：它最后说的东西。
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
// 工具表，是深度的函数
// ---------------------------------------------------------------------------

// tools 返回这个 Agent 可以调用的。
//
// 在深度限制处 `task` 工具被**移除**，不是被拒绝。这是一个刻意的差别，值
// 得一段文字：
//
// 一个运行时拒绝要花一个完整的往返 —— 模型写一个工具调用，宿主拒绝它，模
// 型读拒绝并尝试别的 —— 而且每个永远用不上它的请求，都要搭上工具定义的
// token。更糟的是，这条规则在模型看来明显是随意定的，而模型对付随意规则
// 的办法，就是换个说法继续争辩。
//
// 一个不在列表中的工具不是一个规则。没有东西可争论，没有东西可绕过，模型
// 在它拥有的工具中计划，这正是你想要的。
func (a *agent) tools() []Tool {
	if a.depth >= a.maxDepth {
		return []Tool{bashToolDef()}
	}
	return []Tool{bashToolDef(), taskToolDef()}
}

// ---------------------------------------------------------------------------
// 运行一个回合的工具调用，其中一些同时进行
// ---------------------------------------------------------------------------

// dispatch 执行一个 Assistant 回合中的每个工具调用，并
// **按模型发出的顺序**返回结果。
//
// 子 Agent 调用并发运行；其他所有东西按序运行。顺序保证是要注意的部分：
// 执行是并发的，历史是确定的。如果结果是按完成的顺序追加的，同一个会话重
// 放两次会产生两个不同的消息数组、两个不同的 prompt 前缀，以及 —— 按阶段
// 04 —— 一个永远不会命中的缓存。并发可以改变一件事要花多长时间。它不能
// 改变对话里说了什么。
func (a *agent) dispatch(turn int, calls []Block) ([]Block, bool) {
	results := make([]Block, len(calls))
	texts := make([]string, len(calls))
	stopped := false

	// 第一步：按序工作，和所有东西的权限闸决策。
	//
	// 每个权限问题都在**这里**被问，在一个 goroutine 上，在任何并发开始前。
	// 一个从两个 goroutine 同时写的权限闸 prompt，会在同一行里拼出两句各写
	// 一半的问题，又把同一个答案当成这两句共同的回答——这是一个穿着 UI bug
	// 外衣的安全 bug。
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
			// 一个模型发明的工具名。这确实会发生，答案是把话说得准确而不是让回合失败：
			// 模型能从"没有这样一个工具"中恢复，不能从丢弃的结果恢复。
			texts[i] = fmt.Sprintf("[there is no tool called %q. The tools available to you are listed in this request.]", c.Name)
		}
	}

	// 第二步：子 Agent，全部一次。
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

// runCommand 是 dispatch 的 bash 部分，从阶段 06 开始保持不变，只是它返
// 回渲染的结果而不是追加它。
func (a *agent) runCommand(turn int, callID, command string) string {
	a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
	r := runBash(a.cfg.shell, command, a.cfg.timeout)
	rendered, truncated := r.render(a.cfg.maxOutput)
	a.bus.Emit(Event{
		Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
		ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
		Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
	})
	return rendered
}

// firstLine 是用户在批准子 Agent 时读到的，所以它有两个工作：显示某些东
// 西，永远不要在不完整时读起来完整。
//
// 开头的空白，在切点**之前**而不是之后被修剪。一个以换行开头的 prompt
// —— 大多数时候都是这样，当模型写一个多段落的任务时 —— 过去会产生字符
// 串" …"：省略号前面什么都没有，就出现在正请人授权的那一行上。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}
