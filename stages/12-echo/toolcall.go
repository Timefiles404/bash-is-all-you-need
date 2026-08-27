// 阶段 11——对话里模型写下来、而你非执行不可的，只有工具调用这一样。
//
// 模型产出的其他字段都是文本：文本错了，无非是答得不好。参数不一样——它们要
// 跨进 `exec.Command`，而它们走的那条线，有三种互不相干的办法，把不是模型本
// 意的东西塞给你：
//
//   - 截断。§A2：OpenAI 这条路上，工具调用卡在 max_tokens 上断掉，回来的是
//     `tool_calls: []`，外加网关内部的 `<tool_call><function=bash>` 标记被倒
//     进了 `message.content`。§A3c：Anthropic 这条路上，`input` 被换成
//     `{"raw_arguments": "<invalid JSON>"}`——而 `stop_reason` 照旧写着
//     `"tool_use"`，信封本身一点信号都没有。
//   - 没人执行的 schema。§E13：两条路都不拿返回的调用去对照它收到的那份
//     `input_schema`/`parameters`。`enum` 违规原样回来了，被
//     `additionalProperties: false` 禁掉的属性也原样回来了。
//   - 累加器。参数是碎片过来的，而 §B4 显示切口正落在 token 中间。野生的方言
//     有三种，哪份协议文档都没给其中任何一种起过名字；见 mergeArgs。
//
// 所以这个文件是一道边界：每次调用在被派发之前、**以及**在进入消息数组之前，
// 都要从这儿过一遍。这句话的后半截才是要花钱的那半截：§E14 量了两条路各自怎
// 么对待一次被留下来的坏调用，结果它们朝相反的方向坏。Anthropic 这条路会一直
// 接受它，于是模型要接着往下聊的那场对话里，它看上去用自己从没写过的参数调过
// 一次工具。OpenAI 这条路则**对这个会话里之后的每一次请求都回 400**——而 400
// 判成 fatal 是对的，所以一次没校验的工具调用，就是一个永久报废的会话。
//
// 这个文件有意**不**做的事，是修被截断的参数。这个决定是量出来的，不是断言出
// 来的；见 docs/11-malformed.md。
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 故障分类
// ---------------------------------------------------------------------------

// argFault 是一次工具调用不能派发的原因。
//
// 三个值而不是一个，理由跟阶段 09 有三种分诊裁决一样：它们导向不同的动作，合
// 成一个就把动作丢了。最要紧的区分是 faultCut 和 faultSchema，因为这两个在*到
// 底是谁的错*上说法不同。模型没写完的调用，不等于参数写坏了的调用；明明真相是
// 预算花光了，却告诉模型它的 JSON 非法，就是拿一次往返去换一个错误诊断——模型
// 会把那条同样太长的命令原样再发一遍。
type argFault string

const (
	faultNone argFault = ""

	// faultCut——生成停在了参数中间。判据是网关自己那个 `raw_arguments` 形状
	// （§A3c），或者某个碎片的 JSON 到末尾还*开着*。
	faultCut argFault = "cut"

	// faultNotJSON——东西是到了，是闭合的，但不是 JSON。散文、宿主标记、一句
	// 道歉。
	faultNotJSON argFault = "not_json"

	// faultSchema——合法的 JSON，但违背了本程序就在这一次请求里为这个工具发布
	// 的那份 schema。
	faultSchema argFault = "schema"
)

// argCheck 是这道边界对一次调用的裁决。
type argCheck struct {
	Fault argFault

	// Detail 点出具体是哪一处违规，给 trace 看，也给模型看。绝不放整份载荷：被
	// 截断的参数可以有好几千字节，而接下来每一次请求都要把它重放一遍。
	Detail string

	// Args 是拿去派发的参数，而且**只有** Fault 是 faultNone 时才会被填。这里故
	// 意没有"部分成功"——校验过的调用和没校验过的调用不能是同一个类型，否则只
	// 要有一处调用点忘了看 Fault，那就是一条模型从没写过的 shell 命令。
	Args map[string]any

	// Dropped 列出 schema 没有声明的那些属性，它们是被删掉的，不是被拒的。
	//
	// 这道边界只在这一处有意放宽，理由是笔算。§E13 量到模型是真会加字段——
	// schema 明令禁止的 `timeout_ms`，还是以字符串 "5000" 的形式来的——而上游没
	// 有任何东西拦它们。未知属性按定义就是工具不会去读的属性，删掉它不可能改变
	// 跑起来的东西；拒绝却要搭进一整次往返：模型写，宿主拒，模型读拒绝，模型再
	// 写一遍。为了删掉一个反正会被忽略的键去付这笔钱，这买卖不划算。
	//
	// 它还是要上报，因为另一条路是模型以为自己要的东西和实际发生的事情之间悄悄
	// 分了岔——而模型要一个 `timeout_ms`，这是关于工具设计的一桩事实，不是噪
	// 音。
	Dropped []string
}

// maxDetail 给进入消息数组的东西设了上界。数字本身不重要，有这么个上界才重
// 要。Detail 会跟着历史一路走完这场会话，所以不设上界的 detail，就是为一份没
// 人会再读的证据，付一份不设上界的每回合开销。clip() 是 compact.go 里的那个，
// 两头都保——被截断的参数，有用的那半就在切口上。
const maxDetail = 200

// ---------------------------------------------------------------------------
// checkCall
// ---------------------------------------------------------------------------

// checkCall 决定一次工具调用能不能执行。
//
// `raw` 是累加出来的参数字符串，跟它从线上下来时一模一样；`t` 是**发出去的那
// 份**工具定义——就是塞进请求里的那个 Schema map。收整个 Tool、而不是给每个工
// 具手写一个校验器，要的就是这个：校验和对外声明漂移不了，因为它们是同一个对
// 象。阶段 10 有 `parseBashArgs` 和 `parseTaskArgs`，各自拿 Go 把 bashToolDef
// 和 taskToolDef 早就用 JSON 说过的话再说一遍，而没有任何东西保证它们说的是一
// 回事。
func checkCall(t Tool, raw string) argCheck {
	trimmed := strings.TrimSpace(raw)

	// 零参数的调用。§E14 量到，在 OpenAI 这条路上重放 `arguments: ""` 换来的是
	// HTTP 400，所以空字符串在这道边界上往哪个方向都活不下去——另见 renderArgs。
	if trimmed == "" {
		trimmed = "{}"
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		// 两种"解析不了"的故障，这是哪一种？答案在括号状态里。宽松 JSON 库拿
		// 数括号那套机械去修补，而这个文件用到那套东西*只有*这一处：它在这儿
		// 决定的是怪谁，不是跑什么。
		if jsonIsOpen(trimmed) {
			return argCheck{Fault: faultCut, Detail: clip(trimmed, maxDetail)}
		}
		return argCheck{Fault: faultNotJSON, Detail: clip(trimmed, maxDetail)}
	}

	// 网关自己的截断形状，§A3c。`raw_arguments` 不属于 Anthropic Messages 规
	// 范，所以真有工具用这个名字声明了一个属性，那就是另一回事了——因此这里要
	// 去问 schema，而不是光测一下键在不在。
	if inner, ok := obj["raw_arguments"]; ok && !declaresProperty(t.Schema, "raw_arguments") {
		s, _ := inner.(string)
		return argCheck{Fault: faultCut, Detail: clip(s, maxDetail)}
	}

	if why := schemaViolation(t.Schema, obj); why != "" {
		return argCheck{Fault: faultSchema, Detail: why}
	}
	dropped := pruneUndeclared(t.Schema, obj)
	return argCheck{Args: obj, Dropped: dropped}
}

// pruneUndeclared 删掉 schema 没声明的属性，并把它们的名字返回。只有 schema 真
// 的写了 `additionalProperties: false` 时它才删；工具不吭声，就沿用 JSON
// Schema 的默认值，照单全收。
//
// 所以不管哪种写法，声明都被当真了，而这正是值得拥有的性质：请求里的 schema 和
// 边界上的行为是同一句话，谁也漂移不成一件摆设。
func pruneUndeclared(schema map[string]any, obj map[string]any) []string {
	if allowsExtra(schema) {
		return nil
	}
	props, _ := schema["properties"].(map[string]any)
	var dropped []string
	for _, name := range sortedKeys(obj) {
		if _, declared := props[name]; !declared {
			dropped = append(dropped, name)
			delete(obj, name)
		}
	}
	return dropped
}

// jsonIsOpen 报告文本是不是停在某个值中间：停在字符串里，或者停在容器里。
//
// 它只回答"这是不是被截断了"，别的一概不管。它不是校验器：`{]` 是闭合的，也是
// 胡说八道，它对这个返回 false，这是对的——那属于 faultNotJSON，不是截断。
//
// 它以前还查末尾的逗号和冒号，变异测试把那段删了：没有任何输入能走到那儿。切口
// 紧跟在 `{"a":` 或 `{"a":1,` 后面时，对象是开着的，所以 `depth > 0` 早就把话
// 说完了；而不在容器里的值，根本不可能含有逗号或冒号。两行代码，加一个永远改
// 不了答案的变量。
func jsonIsOpen(s string) bool {
	depth := 0
	inStr, esc := false, false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}

	// 末尾孤零零一个反斜杠，是个没东西可转义的转义符；只有切口正好落在那两个字
	// 节中间时才会这样。
	return inStr || esc || depth > 0
}

// ---------------------------------------------------------------------------
// schema 的子集
// ---------------------------------------------------------------------------

// schemaViolation 拿 `obj` 去比对 `schema`，把第一处违规写成一句话返回；没有就
// 返回 ""。
//
// 这**不是** JSON Schema 的实现，也不该变成一个。它覆盖的关键字，正好就是这个
// 仓库的工具声明过的那几个——`type`、`properties`、`required`、`enum`、
// `additionalProperties`——因为校验器要是懂你从来不发的关键字，那它就是你自己写
// 的一个依赖，而且它会在没人测过的地方跟真家伙意见不合。
//
// 有意漏掉两样，两样都是更严格的校验器会抓到、而这里留给工具去管的：
//
//   - 数字只查是不是数字，不查是不是整数、在不在范围内。声明的是 5，模型发
//     5.0；上限是 100，模型发 200——而拒绝要搭进一整次往返（模型写、宿主拒、
//     模型读、模型重试），换来的事情一个 clamp 免费就办了。边界上要多严，是延
//     迟和成本的决定，不只是正确性的决定。这个文件定下的规矩是：只有一种显而易
//     见读法的就迁就，没有的就拒。
//   - 嵌套对象属性。这里没有哪个工具有，而会递归的 schema 遍历器，就是等着出环
//     bug 的 schema 遍历器。
//
// 这一切之所以存在，全因为 §E13：enum 明明是 `["bash","sh"]`，端点却返回了
// `"shell": "powershell"`，还带回一个被 `additionalProperties: false` 禁掉的属
// 性，两次都是 200，finish reason 也正常。上游没有任何东西在做这件事。
func schemaViolation(schema map[string]any, obj map[string]any) string {
	props, _ := schema["properties"].(map[string]any)

	// 先查 required，而且缺 required 排在类型不对前面，因为被截断但还能解析的调
	// 用——§E14 里那个 `{}`——属于缺，不属于错；而"command 字段不在"这句话，比
	// 一张"其余全都没问题"的清单有用得多。
	for _, name := range requiredNames(schema) {
		if _, ok := obj[name]; !ok {
			return fmt.Sprintf("the required %q field is absent", name)
		}
	}

	// 排过序，因为 Go 里 map 的遍历顺序是随机的；而每跑一次就点到另一个字段的
	// 错误消息，是一份没人复现得了的 bug 报告。
	for _, name := range sortedKeys(obj) {
		value := obj[name]
		spec, declared := props[name].(map[string]any)
		if !declared {
			// 这由 pruneUndeclared 管，不在这儿：没声明的属性不是模型需
			// 要听到的违规，而是工具不会去读的键。见 argCheck.Dropped。
			continue
		}
		if want, ok := spec["type"].(string); ok {
			if got := jsonTypeOf(value); !typeMatches(want, got) {
				return fmt.Sprintf("%q should be %s and arrived as %s", name, want, got)
			}
		}
		if allowed, ok := spec["enum"].([]any); ok && !inEnum(value, allowed) {
			return fmt.Sprintf("%q was %v, which is not one of %v", name, value, allowed)
		}
	}
	return ""
}

// requiredNames 读 `required` 列表，[]string（本仓库的工具定义写出来的那种）和
// []any（JSON 走一趟往返产出的那种）它都收。两种形状同时存在，是因为在 Go 里搭
// 出来的 Tool 和从配置文件解出来的 Tool 是同一个类型；而只认得其中一种的校验
// 器，在测试里能用，在重放里不能用。
func requiredNames(schema map[string]any) []string {
	switch req := schema["required"].(type) {
	case []string:
		return req
	case []any:
		out := make([]string, 0, len(req))
		for _, r := range req {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// allowsExtra 默认为 true——就是 JSON Schema 的默认值——所以从没提过
// additionalProperties 的工具，会继续收下模型偶尔多加的那个无害字段，而不是一
// 声不响地变严。
func allowsExtra(schema map[string]any) bool {
	if v, ok := schema["additionalProperties"].(bool); ok {
		return v
	}
	return true
}

func declaresProperty(schema map[string]any, name string) bool {
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[name]
	return ok
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonTypeOf 说出一个解进 `any` 的值是什么 JSON 类型。注意 json.Unmarshal 已经
// 把整数和浮点都塌成了 float64，所以 "integer" 在这里根本观察不到——这就是不查
// 整数范围的那个原因里，机械的那一半。
func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeMatches(want, got string) bool {
	if want == "integer" {
		return got == "number"
	}
	return want == got
}

func inEnum(v any, allowed []any) bool {
	for _, a := range allowed {
		if a == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 告诉模型什么
// ---------------------------------------------------------------------------

// faultText 是被拒的调用拿到的工具结果。
//
// 这里每一个字符串都归同一条规矩管，而这条规矩并不显然：**这段文字只描述发生了
// 什么，不下任何指令。**不写 "send valid JSON"，不写 "try again with a shorter
// command"，不写 "do not retry this unchanged"。
//
// 理由是，这段文字不是一条消息。它是往 prompt 上做的永久追加：它会进消息数组，
// 这个会话里之后的每一次请求都会把它再发一遍。搁在里面的祈使句，几个回合之后、
// 在它的上下文已经没了的时候，读起来就是一条新指令——于是模型把一次已经处理过
// 的调用又发一遍，而且消息越旧，它发得越理直气壮。阶段 10 出厂时带了四条这样的
// 话（"send valid JSON"、"send it again"、"Retry with a shorter command"、"Do
// not retry it unchanged"），现在都没了。
//
// 一句事实，放旧了还是一句事实。
func faultText(t Tool, c argCheck) string {
	switch c.Fault {
	case faultCut:
		// 故意不把碎片引回去。它是模型自己写的，没必要再给它看一遍；何况
		// §A3c 的碎片能有好几百字节的 shell 命令，一引就要永远重放下去。
		return fmt.Sprintf("[not executed: the arguments for %s stopped mid-value, "+
			"so the call never finished being written]", t.Name)
	case faultNotJSON:
		return fmt.Sprintf("[not executed: the arguments for %s were not JSON. "+
			"What arrived: %s]", t.Name, c.Detail)
	case faultSchema:
		return fmt.Sprintf("[not executed: %s]", c.Detail)
	}
	return ""
}

// ---------------------------------------------------------------------------
// 身份
// ---------------------------------------------------------------------------

// uniqueIDs 让整场会话里每个工具调用 id 都互不相同，就地给撞车的改名，并报告改
// 了几个。
//
// 它之所以存在，是因为网关可以给它发出的每一次调用都铸同一个 id。在一个回合内
// 这是合法的——除了配对的那条结果，没人读这个 id——而历史一被重放，它就是致命
// 的：协议要求 id 在整个消息数组里唯一，而拒绝消息点的是消息下标、不是工具，于
// 是错误指向了错的地方。
//
// 有两条性质值得说出来，因为走到这一步的路上两条都出过 bug：
//
//   - `seen` 跨的是会话，不是回合。真正要命的重复住在*不同的* assistant 消息
//     里，所以按回合查什么也查不出来，请求照样被拒。
//   - 改名发生在结果块被造出来**之前**，这样调用和它的答复才带着同一个新 id。
//     给一次结果已经存在的调用改名，就是把"id 重复"的拒绝换成"结果孤立"的拒
//     绝——同一场故障，换了条更没用的消息。
func uniqueIDs(calls []Block, seen map[string]bool) int {
	renamed := 0
	for i := range calls {
		if calls[i].Kind != BlockToolCall {
			continue
		}
		id := calls[i].ID
		if id == "" {
			// 结果就是靠 id 找到自己那次调用的。没有 id 的调用没法被答
			// 复，而没被答复的调用，就是一次被拒的请求。
			id = fmt.Sprintf("call_%d", len(seen)+1)
		}
		if !seen[id] {
			seen[id] = true
			calls[i].ID = id
			continue
		}
		for n := 2; ; n++ {
			// 后缀有上界，为的是让 id 待在网关会校验的 64 字符限制之
			// 内；这里观察到的 id 是 24-28 个字符，位置够；而反过来去截
			// *前缀*，有可能正好撞上要躲开的那个 id。
			cand := fmt.Sprintf("%s_%d", id, n)
			if len(cand) > 56 {
				cand = fmt.Sprintf("call_dup_%d", len(seen)+1)
			}
			if !seen[cand] {
				seen[cand] = true
				calls[i].ID = cand
				renamed++
				break
			}
		}
	}
	return renamed
}

// ---------------------------------------------------------------------------
// 宿主标记的泄漏
// ---------------------------------------------------------------------------

// harnessMarkers 是网关内部那套工具调用语法的起手宿主标记，照 §A2 抓到的样子：
// `<tool_call>\n<function=bash>\n<parameter=command>…`。
var harnessMarkers = []string{"<tool_call>", "<function=", "<parameter="}

// stripHarnessMarkup 把漏进 assistant 文本里的网关内部货色删掉，并说明有没有找
// 到过。
//
// 机制在 §A2：模型在线上发的根本不是 JSON，而是这套类 XML 的语法，网关在服务端
// 把它解析成 `tool_calls`。生成停在语法中间时解析失败，网关就退而求其次，把原
// 始宿主标记塞在 `message.content` 里交给你。于是一下子出了两处失败，阶段 10 两处
// 都占：这段宿主标记被当成 assistant 说的话打给人看，又被当成 assistant 文本追加进
// 历史——在那里它教会模型，把这套语法当散文发出来，在这儿是件会发生的事。
//
// 它从第一个宿主标记一直切到末尾，而不是去挖出一个配平的块，因为根本没有配平的块：
// 这段文本按定义就是被截断的，缺的正好就是闭合标签。
//
// 它**不**做的事，是把这段宿主标记解析成一次工具调用。碎片里常常有一个看上去很完
// 整的 `<parameter=command>` 值，而把它跑起来，跟修被截断的 JSON 是同一个错
// 误，只是多绕了一步：模型*在谈论*一次工具调用和模型*在发起*一次工具调用，会
// 变得分不出来；而那份定音的证据——网关自己都没能解析它——是反对执行的证据，不
// 是支持执行的。
func stripHarnessMarkup(text string) (string, bool) {
	cut := -1
	for _, m := range harnessMarkers {
		if i := strings.Index(text, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return text, false
	}
	return strings.TrimRight(text[:cut], " \t\n"), true
}

// ---------------------------------------------------------------------------
// 累加器的方言
// ---------------------------------------------------------------------------

// mergeArgs 把流式到达的一片参数，并进已经累加下来的东西里。
//
// 无条件追加是显而易见的实现，而它是错的，因为这个字段上会到三种不同的方言，没
// 有任何一份协议文档给其中任何一种起过名字：
//
//	incremental  每一片是接下来的几个字节         -> append
//	cumulative   每一片是到目前为止的整个值       -> replace
//	re-send      最后一片重复整个值               -> replace
//
// §B4 显示这个端点上跑的是 incremental 方言，切口落在 token 中间，所以 append
// 是对的默认。另外两种是真实存在的：网关要是在最后一个 chunk 里把完整参数重发
// 一遍，追加就会变成 `{"command":"ls"}{"command":"ls"}`，而它的报错——`invalid
// character '{' after top-level value`——只点出字节偏移量，对成因只字未提。
//
// 把它们分开的判据是：**工具调用的参数正好是一个顶层 JSON 值**，所以"缓冲区
// 已经能解析了"就是个终态。在那之后到的任何东西，都不可能是续写。
func mergeArgs(have, frag string) string {
	if frag == "" {
		return have
	}
	if have == "" {
		return frag
	}
	// 终态：手上这份已经是完整的值了。
	if json.Valid([]byte(strings.TrimSpace(have))) {
		// 最后一个完整值胜出；末尾的残片扔掉。留第一个同样说得通，实际效果
		// 却更差——re-send 这种方言之所以存在，恰恰是因为网关认定它最后那个
		// chunk 才是权威的。
		if json.Valid([]byte(strings.TrimSpace(frag))) {
			return frag
		}
		return have
	}
	// Cumulative：这一片把已经有的全都包在里面了。
	if strings.HasPrefix(frag, have) {
		return frag
	}
	return have + frag
}

// ---------------------------------------------------------------------------
// 读一次校验过的调用
// ---------------------------------------------------------------------------

// toolByName 找出这道边界要拿来校验的那份定义。
//
// 它收的是 Agent *当下*正在对外声明的那张列表，不是包级全局变量，因为那张列表
// 是深度的函数：到了递归上限，`task` 工具会被整个从请求里拿掉（见 tools()）。
// 拿全局表去校验，就会接受一次调用，而它要的工具这个 Agent 压根没提供过——而这
// 恰恰是"没有这个工具"才是老实答案的唯一一种情形。
func toolByName(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// argsForDisplay 是一次工具调用的参数最可读的形式，给 TUI 和上下文压缩摘要用。
//
// **它的结果绝不能走到执行**，正是这句话让它成了单独的函数，名字也这么写着。
// 它恰恰就是这个文件其余部分拒绝的那种宽松解析：能拿到什么就拿什么，拿不到就退
// 回原始字节——因为查看器显示一条烂掉的命令是对的，什么都不显示才不对。同样的宽
// 松放到派发路径上，被截断的命令就是这么跑起来的。
//
// 它专门读 `command`，而不是读第一个字符串属性，因为那样一来，将来某个工具碰巧
// 先声明的字段，就会被一声不响地捧成人眼里的"那条命令"。
// 它也从不返回空白。`{"raw_arguments":""}` 和 `{"command":"  "}` 都是真实的载
// 荷（前者是 §A3c 的，后者是模型发出了一条全是空白的命令），从这两个里抽出来
// 都是空字符串——而空字符串放在面板上、放在上下文压缩摘要里，就是一次凭空消失
// 的工具调用。退回原始字节，留住的是*有过*尝试这份证据，而查看器之所以要宽松
// 解析，图的就是这个。
func argsForDisplay(args string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(args)), &obj); err == nil {
		for _, key := range []string{"command", "raw_arguments"} {
			if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return args
}

// strArg 从一次校验过的调用里读出一个字符串属性。
//
// 类型断言之所以安全，只因为 schemaViolation 已经跑过了；正是这层耦合，让它收
// 的是 argCheck 而不是光秃秃的 map：调用方要拿到参数，就必须先过那道让断言成立
// 的边界。
func strArg(c argCheck, name string) string {
	s, _ := c.Args[name].(string)
	return s
}

// renderArgs 决定一次工具调用的参数在线上长什么样。
//
// 零参数的调用发 `{}` 而不是 `""`，因为 §E14 量到 OpenAI 这条路上
// `arguments: ""` 换来的是 HTTP 400，而 `{}` 是被接受的——而且空字符串不是假
// 想：§B4 显示流式的第一个 `tool_calls` delta 到手时带的就是
// `"arguments":""`，所以一条在宣告和第一个碎片之间断掉的流，累加出来的正好就
// 是它。
//
// Anthropic 那一侧一直是这么做的（见 anthropicToolInput）；这里是对称的另一
// 半，阶段 10 没有。
func renderArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	return args
}
