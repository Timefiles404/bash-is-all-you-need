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
	"context"
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
			// `description` **不是**必填的，阶段 11 就是改正这件事的地方。阶段 07
			// 把两个字段都声明成必填，然后在标签缺失时把它兜成 "subtask"——于是
			// 模型被告知某个字段是强制的，却又被一条根本不需要它的规则来判。这个
			// 偏差没人抓住，因为 schema 只被单向地跟解析器比过：测试问的是"解析
			// 器要的，schema 是不是都要求了？"，从来没问过"schema 要的，解析器是
			// 不是都需要？"
			//
			// 这是个看着不值钱、代价却实打实的谎。模型要是信了这个标签是强制的，
			// 就会在标签毫无意义的调用上花 token 去编一个；而省掉它的模型——对一
			// 句话的任务来说，省掉才是对的——碰上比阶段 07 手里那个更严格的校验
			// 器，就会被拒。
			"required":             []string{"prompt"},
			"additionalProperties": false,
		},
	}
}

// taskDescription 是子 Agent 跑起来时挂的那个标签。
//
// `description` 声明了但不必填，所以一次过了检查的调用完全可以不带它；而没有标
// 签的子 Agent 仍然是该跑起来的子 Agent。这是 emptyCommand 的对应物：schema
// 说什么必须在，工具说那些只是"允许"的东西该怎么办。
func taskDescription(c argCheck) string {
	if d := strings.TrimSpace(strArg(c, "description")); d != "" {
		return d
	}
	return "subtask"
}

// emptyPrompt 是 task 这边的 emptyCommand：管这个值是什么意思的那道检查。
//
// `{"prompt": ""}` 满足 schema——字段在，是字符串——然后起一个没有任务的子
// Agent，它会烧掉一整个上下文窗口去弄明白自己无事可做，再报回一段听着挺像那
// 么回事的东西。schema 关键字表达不了"不许空白"，所以由工具来说。
func emptyPrompt(c argCheck) bool {
	return strings.TrimSpace(strArg(c, "prompt")) == ""
}

// ---------------------------------------------------------------------------
// 派生
// ---------------------------------------------------------------------------

// spawn 把一个子 Agent 跑到结束，返回它的报告。
//
// 注意什么共享、什么不共享。共享的：provider、HTTP 客户端、权限闸、
// shell 配置，还有总线的 core——这样子 Agent 的权限提问会送到同一个
// 人面前，它的事件也落进同一份有序的 trace。不共享的：消息数组、系
// 统提示词、compactor 和回合预算。这道分界线恰好就是"父 Agent 不能丢的
// 状态"对上"子 Agent 不能继承的状态"。
func (a *agent) spawn(ctx context.Context, callID, description, prompt string) (string, Usage, error) {
	started := time.Now()
	agentID := fmt.Sprintf("%s#%d", description, a.nextChild())

	a.bus.Emit(Event{
		Kind: KindSubagentStart, ToolID: callID,
		ToolName: description, Text: prompt,
	})

	child := a.newChild(agentID, func() string { return subagentSystem + para + a.stable })

	msgs := []Msg{TextMsg(RoleUser, prompt)}
	msgs = child.runTurn(ctx, msgs)

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
// 共享的：provider、HTTP 客户端、权限闸、shell 配置，还有总线的
// core——这样子 Agent 的权限提问会送到同一个人面前，它的事件也落进同
// 一份有序的 trace。不共享的：消息数组、系统提示词、compactor 和回合预
// 算。
//
// 一个字段一个字段写出来，而不是写 `child := *a`——后者更短，而且
// `go vet` 拒得没错：agent 里有 sync.Mutex，复制含互斥锁的结构体，副
// 本拿到的锁就停在原件当时的那个状态上。写全了也更诚实：这里每一
// 行，都是在决定子 Agent 到底是什么。
func (a *agent) newChild(agentID string, system func() string) *agent {
	child := &agent{
		// 梯子是按指针共享的，不是拷贝的，而这是个决定，不是语法上的意外。
		// "这个端点在拒绝调用"是关于端点的事实，所以子 Agent 发现了它，不该
		// 还要回头教父 Agent；而已经降级过的父 Agent，也不该把自己的子 Agent
		// 送回已经死掉的那一级。这也是 ladder 要带一把互斥锁的原因：好几个子
		// Agent 可能同时在同一家供应商上失败。
		lad: a.lad, pol: a.pol, httpc: a.httpc, g: a.g, cfg: a.cfg,

		// 阶段 10 发布的时候这里漏了 dl，而且漏得一声不响：deadlines 结构体全
		// 零就意味着每一个时钟都是关的（见 guardBody 和 waitFor，两处都把 <= 0
		// 当成"没有看门狗"），于是子 Agent 跑起来既没有卡住检测，也没有整次调
		// 用的兜底，而父 Agent 两样都有。没有任何东西失败，也没有任何东西说一
		// 声——而永远挂住的那个子 Agent，恰恰是阶段 10 存在的理由，也恰恰是当
		// 时仍然暴露在它面前的那一个。
		dl: a.dl,

		// 全新的一套 id 集合，不是父 Agent 那套。子 Agent 有自己的消息数组，它
		// 的 id 只要在里面唯一就行——而共用这张 map，并发的几个子 Agent 每次调
		// 用都得去抢它。
		seenIDs: map[string]bool{},

		// 结果缓存是共享的，跟 ladder 一样，跟 compactor 不一样，判断标准是
		// 这件事是谁的事实。"这个端点在拒绝调用"是端点的事实。"这个文件里是
		// 这些字节"是工作树的事实，而父 Agent 和每个子 Agent 同时都在看着它。
		// 每个子 Agent 一份缓存也是对的，代价是它会漏掉结果缓存唯一真正划算
		// 的场景：三个子 Agent 在同一秒里打开同一个文件。
		//
		// 阶段 10 在相反的疏忽上丢过一整个功能——`dl` 就是没写进这个结构体字
		// 面量，于是子 Agent 跑的时候每个时钟都是关的，什么都没坏，也什么都
		// 没说。这一行之所以在这里，就是因为那一行当时不在。
		echo: a.echo,

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

// dispatchOutcome 是主循环关于这一回合的工具调用、除了结果本身之外还需要知道的
// 东西。
//
// `cut` 的存在来自一次实测，不是一个猜想。阶段 11 的边界把被截断的调用分类分对
// 了，也拒掉了，然后一次跑在 `--max-tokens 110` 下的真实会话**发起了十六次模型
// 调用，跑了零条命令**：每一次都被截断，模型每一次都被告知了，而它的回答是再写
// 一条同样长的命令。最后只有回合预算拦住了它。
//
// 这就是"告诉模型"的极限。它没法靠换个说法修好截断，因为问题出在预算上，而模型
// 看不见预算。所以这个计数离开 dispatch，变成一根保险丝——见 runTurn。
type dispatchOutcome struct {
	stop  bool // 人中止了这次会话
	calls int
	cut   int // 按 faultCut 拒掉的调用数
}

// dispatch 执行一个 assistant 回合里的每一次工具调用，并且**按模型发
// 出的顺序**返回结果。
//
// 子 Agent 的调用并发跑，其余的按顺序跑。要留意的是那条顺序保证：执
// 行是并发的，历史是确定的。结果要是按完成先后追加，同一次会话重放
// 两遍会产出两份不同的消息数组、两份不同的 prompt 前缀，以及——照阶
// 段 04 的说法——永远命不中的缓存。并发可以改变事情花多长时间，不可
// 以改变对话说了什么。
func (a *agent) dispatch(ctx context.Context, turn int, calls []Block) ([]Block, dispatchOutcome) {
	results := make([]Block, len(calls))
	texts := make([]string, len(calls))
	out := dispatchOutcome{calls: len(calls)}
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

	offered := a.tools()

	for i, c := range calls {
		if stopped {
			texts[i] = "[not executed: the session was stopped]"
			continue
		}

		// 阶段 11 的那道边界，注意它待的位置：在 switch 之前，不在每个 case 里
		// 面。每一个工具都要过它，包括以后被某个从没读过这个文件的人加进来的工
		// 具——这正是它做成"一个接受 Tool 的函数"、而不是"每个工具一份校验器"
		// 的全部理由。
		def, known := toolByName(offered, c.Name)
		if !known {
			// 模型编出来的工具名。这种事会发生，而对策是把话说准，而
			// 不是让整个回合失败：模型能从"没有这个工具"里恢复过来，
			// 从丢掉的结果里恢复不了。
			texts[i] = fmt.Sprintf("[there is no tool called %q. The tools available to you are listed in this request]", c.Name)
			continue
		}
		checked := checkCall(def, c.Args)
		if checked.Fault != faultNone {
			a.bus.Emit(Event{
				Kind: KindToolCallInvalid, Turn: turn, ToolID: c.ID, ToolName: c.Name,
				Fault: string(checked.Fault), Text: checked.Detail,
			})
			if checked.Fault == faultCut {
				out.cut++
			}
			texts[i] = faultText(def, checked)
			continue
		}
		if len(checked.Dropped) > 0 {
			// 发的是 notice，不是工具结果。模型要了这个工具没有的东西，这值得让
			// 人看见——但把它写进工具结果，就是拿历史去养一个什么都没改变的键，
			// 往后每一次请求都带上，永远。
			a.bus.Notice("%s: ignored undeclared argument(s) %s",
				c.Name, strings.Join(checked.Dropped, ", "))
		}

		switch c.Name {
		case "task":
			if emptyPrompt(checked) {
				a.bus.Emit(Event{
					Kind: KindToolCallInvalid, Turn: turn, ToolID: c.ID, ToolName: c.Name,
					Fault: string(faultSchema), Text: "blank prompt",
				})
				texts[i] = "[not executed: the prompt was blank, so there was no task to delegate]"
				continue
			}
			description, prompt := taskDescription(checked), strArg(checked, "prompt")
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name,
				Command: description + ": " + firstLine(prompt)})
			v, why := a.g.ask("subagent — " + description)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this subagent]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session]"
			default:
				async = append(async, pending{i, description, prompt})
			}

		case "bash":
			command := strArg(checked, "command")
			if emptyCommand(command) {
				a.bus.Emit(Event{
					Kind: KindToolCallInvalid, Turn: turn, ToolID: c.ID, ToolName: c.Name,
					Fault: string(faultSchema), Text: "empty command",
				})
				texts[i] = "[not executed: the command was an empty string]"
				continue
			}
			a.bus.Emit(Event{Kind: KindToolCallReady, Turn: turn, ToolID: c.ID, ToolName: c.Name, Command: command})
			v, why := a.g.ask(command)
			a.bus.Emit(Event{Kind: KindGateVerdict, Turn: turn, ToolID: c.ID, Verdict: string(v), Text: why})
			switch v {
			case deny:
				texts[i] = "[the user denied this command]"
			case abort:
				stopped = true
				texts[i] = "[the user stopped the session]"
			default:
				texts[i] = a.runCommand(ctx, turn, c.ID, command)
			}

		}
		// 没有 default 分支：不认识的名字在 switch 之前就已经被回答过了，所以
		// 带着一个没人处理的名字走到这里是不可能的，除非 tools() 对外宣称了
		// dispatch 跑不了的东西——那会是这个文件里的 bug，不是要拿去跟模型解
		// 释的事。
	}

	// 第 2 趟：子 Agent，一起上。
	if len(async) > 0 {
		var wg sync.WaitGroup
		for _, p := range async {
			wg.Add(1)
			go func(p pending) {
				defer wg.Done()
				report, _, err := a.spawn(ctx, calls[p.i].ID, p.description, p.prompt)
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
	out.stop = stopped
	return results, out
}

// runCommand 是 dispatch 的 bash 那一半。阶段 12 在它前面加了一个问题：这个
// 答案我们是不是已经知道了？
//
// 注意问的位置——在权限闸之后，不是之前。命中同样是一堆字节送到模型面前，而
// "你以前见过这个"跟"你可以看这个"不是同一种许可。把查询挪到闸门之前，一对
// 命令里的第二条就再也批不动了——一个越跑越松的权限系统。
func (a *agent) runCommand(ctx context.Context, turn int, callID, command string) string {
	look := a.echo.lookup(a.cfg.shell, a.cfg.wd, command, a.cfg.maxOutput, a.cfg.env)
	if look.verdict == cacheHit {
		a.bus.Emit(Event{
			Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
			Verdict: string(cacheHit), Bytes: len(look.text), Millis: look.millis,
		})
		return look.text
	}
	if a.echo != nil {
		a.bus.Emit(Event{
			Kind: KindResultCache, Turn: turn, ToolID: callID, Command: command,
			Verdict: string(look.verdict), Text: look.reason,
		})
	}

	a.bus.Emit(Event{Kind: KindCommandStart, Turn: turn, ToolID: callID, Command: command})
	r := runBash(ctx, a.cfg.shell, command, a.cfg.timeout)
	rendered, truncated := r.render(a.cfg.maxOutput)
	a.bus.Emit(Event{
		Kind: KindCommandEnd, Turn: turn, ToolID: callID, Command: command,
		ExitCode: r.ExitCode, TimedOut: r.TimedOut, Truncated: truncated,
		Bytes: len(rendered), Millis: r.Duration.Milliseconds(),
	})

	// 放在事件之后存，所以哪怕存被拒了——而它多半会被拒——trace 里记的也是真
	// 实发生过的事。整个 lookup 原样传回去，而不是只传它的 key：它身上带着
	// 这些见证文件在这条命令读它们**之前**的摘要，store() 要拿它跟现在的摘
	// 要比。
	a.echo.store(look, command, rendered, r)
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
