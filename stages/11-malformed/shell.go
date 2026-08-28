// 阶段 11——把交互式外壳接上来。
//
// 这个文件不是课程的一部分，它把东西交出去的那个 tui/ 包也不是。没有任何一
// 章讲它们。它们存在，是因为"读一个阶段"和"在一个阶段里*干活*"要的是两个不
// 同的程序：各章要的界面得小到能整个装进脑子里，而拿分诊或者工具调用边界去
// 戳的人要的正好相反：一个不会因为少了个配置项就关掉的窗口、一个按下去就能
// 停住失控回合的键、一条不用编辑文件就能换端点的路。
//
// 所以外壳住在 tui/ 里，这个文件是那道缝。这道缝值得留意的地方是它有多窄：
// 阶段 02 写的渲染器被指到另一个 io.Writer 上，阶段 01 写的权限闸改从一个函
// 数而不是 Scanner 取答案，Agent 里除此之外没有任何东西知道有界面存在。这不
// 是运气。这是阶段 02 在 Agent 和所有盯着它的东西之间塞进一条事件总线时买下
// 的，而回报是：九个阶段之后整个前端接了进来，那条总线一行都没改。
//
// 外壳在会话运行中能改的每样东西，都是下面那个结构体的字段，而不是 main()
// 里的局部变量——这是唯一一处结构上的代价。在 main() 里算一次、再被闭包抓走
// 的值，是 /open 改不动的值；而一个 Agent 自称在某个工作目录、命令却不在那
// 里跑，比根本没有 /open 更糟。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bash-is-all-you-need/tui"
	"bash-is-all-you-need/tui/settings"
)

// shellOpts 是外壳重建某样它自己改过的东西时要用的那些命令行取值。这里存的
// 是拷贝而不是引用，这样一个 flag 的值和会话当前的值就不会悄悄各走各的。
type shellOpts struct {
	provider   string
	fallback   string
	cacheBP    bool // cache_control 断点；--no-cache 的反面
	window     int
	noMemory   bool
	noSkills   bool
	breakCache bool
}

// shellSession 是一场交互式会话。
type shellSession struct {
	pf    *providersFile
	view  *renderer
	bus   *Bus
	app   *tui.App
	store *settings.Store
	trace *traceSink
	folds *foldSink
	opts  shellOpts

	// storeErr 记的是没有设置存储时，它为什么没有。它进的是横幅而不是日志，
	// 因为被它禁掉的那些命令，正是手上没有环境的用户最先要伸手去拿的那几条。
	storeErr error

	// mu 守着它下面的所有东西。外壳一次只跑一个回合或一条命令，所以争的不是
	// 两个回合之间——争的是回合和状态栏之间：回合正往 msgs 里追加，而状态栏在
	// 另一个 goroutine 上一秒重画三十次。
	mu    sync.Mutex
	a     *agent
	pname string
	pcfg  providerConfig
	msgs  []Msg

	// wd 是命令跑起来的地方。
	//
	// config 里没有工作目录这个字段，也不需要有：没有任何东西设过 cmd.Dir，
	// 所以决定一条命令在哪里跑的是进程自己的目录，而挪动它的是 open() 里那句
	// os.Chdir。凡是要*报告*目录的地方读的都是这个字段，而 open() 是两者唯一
	// 的写入方。
	wd string
}

// ---------------------------------------------------------------------------
// 可切换的 trace
// ---------------------------------------------------------------------------

// traceSink 是总线上一个永不退订的订阅者，只是它写的那个文件可以换。
//
// 总线没有 Unsubscribe，也不该有：一份能缩短的订阅者名单，意味着 trace 可以
// 在会话中途被摘掉，而那个文件从此描述的是一场没发生过的会话。所以 /trace
// 换的是这个永不离场的订阅者背后的文件。
type traceSink struct {
	mu sync.Mutex
	w  *TraceWriter
}

func (t *traceSink) OnEvent(e Event) {
	t.mu.Lock()
	w := t.w
	t.mu.Unlock()
	if w != nil {
		w.OnEvent(e)
	}
}

func (t *traceSink) open(path string) error {
	w, err := NewTraceWriter(path)
	if err != nil {
		return err
	}
	t.mu.Lock()
	old := t.w
	t.w = w
	t.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func (t *traceSink) close() {
	t.mu.Lock()
	w := t.w
	t.w = nil
	t.mu.Unlock()
	if w != nil {
		w.Close()
	}
}

func (t *traceSink) path() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.w == nil {
		return ""
	}
	return t.w.Path()
}

// ---------------------------------------------------------------------------
// 折叠
// ---------------------------------------------------------------------------

// foldSink 告诉输出区，渲染器接下来要写的是哪一类行。
//
// 它必须排在渲染器*前面*订阅，而 main() 的做法是把它写成 NewBus 的第一个参
// 数。Emit 是在 core 的那把 mutex 底下按顺序派发给每个订阅者的，所以轮到渲
// 染器时，它即将写出的那些行属于哪一类，早已定好。要是登记在渲染器后面，这
// 段代码照样编译得过、照样跑得起来，只是每一行都会被标成它前面那个事件的类
// 别——这种错在屏幕安静的时候看着是对的，会话一忙起来就全乱了。
//
// 另一条路是照渲染出来的文字的前缀去分类。那样今天也能用，连总线订阅者都不
// 用要，然后在某个人改动 render.go 里某个字符的那一刻悄悄失效。事件本身就知
// 道答案，没有理由再从画面上把它倒推一遍。
type foldSink struct {
	mu  sync.Mutex
	app *tui.App
}

// attach 等外壳建起来之后，才把外壳交给这个订阅者。在那之前发出的事件——启动
// 时那行供应商——就干脆不分类，这也是对的：它们属于横幅，而横幅从不折叠。
func (f *foldSink) attach(app *tui.App) {
	f.mu.Lock()
	f.app = app
	f.mu.Unlock()
}

func (f *foldSink) OnEvent(e Event) {
	f.mu.Lock()
	app := f.app
	f.mu.Unlock()
	if app != nil {
		app.SetClass(classOf(e))
	}
}

// classOf 决定精简视图留下什么。
//
// 它写下的规矩是：Agent 说了什么、做了什么，留着；仪器量出来的那些，折起
// 来。于是精简视图里剩下的，是模型的行文、每条命令在跑之前的样子，以及所有
// 出错的地方——也就是一个人复述这场会话时会讲的那些。折起来的是每次调用的那
// 块仪表盘、每条命令的原始输出，还有围着它们的那些账：这些恰恰是这个仓库存
// 在的理由，却没有一样是你在等着看 Agent 有没有听懂你的时候想留在屏幕上的。
func classOf(e Event) tui.Class {
	switch e.Kind {
	case KindTextDelta:
		return tui.ClassProse

	case KindToolResult, KindResponseEnd, KindUsage, KindRetry,
		KindMemoryLoaded, KindSkillsIndexed, KindCompactStart, KindCompactEnd,
		KindRequest:
		return tui.ClassDetail

	default:
		// 别的都留在看得见的那一侧，而默认值特意选的就是看得见这一边。以后往
		// events.go 里加的新 Kind 会一直显示在屏幕上，直到有人另做决定——这是那
		// 种会被人注意到的失败。
		return tui.ClassPlain
	}
}

// ---------------------------------------------------------------------------
// 跑起来
// ---------------------------------------------------------------------------

func (s *shellSession) run(ctx context.Context) error {
	s.app = tui.New(tui.Config{
		Title:          "stage 11",
		Banner:         s.banner(),
		Submit:         s.submit,
		Commands:       s.commands(),
		Status:         s.status,
		Segments:       s.segments,
		Ready:          s.ready,
		Open:           s.open,
		Reconfigure:    s.reconfigure,
		Settings:       s.store,
		Env:            tui.EnvNames{BaseURL: "AGENT_BASE_URL", APIKey: "AGENT_API_KEY", Protocol: "AGENT_PROTOCOL", Model: "AGENT_MODEL"},
		InterruptCause: errInterrupted,
		OnExit:         s.onExit,
	})
	s.folds.attach(s.app)
	// 渲染器从 stdout 挪到外壳的输出区，输出这一侧的改动就这么多。还往 stdout
	// 写的东西会落在备用屏上、帧的底下，把帧毁掉——agent.out 就是为这个存在
	// 的，command() 也是为这个往那里写。
	s.view.out = s.app.Out()
	s.mu.Lock()
	s.a.out = s.app.Out()
	s.a.g.out = s.app.Out()
	s.a.g.read = s.gateAsk
	s.mu.Unlock()
	return s.app.Run(ctx)
}

// gateAsk 是权限闸的读行函数，取代它从阶段 01 一路用到现在的那个 Scanner。
//
// 权限闸不能再读 stdin 了：外壳占着它，而且是原始模式，同一个描述符上再挂一
// 个 Scanner，会把用户正在敲的那一行里的按键抢走。于是问题交给外壳去问，回
// 来的是外壳自己收到的答复。这里返回 false 意思是外壳正在关闭，而权限闸处理
// 这种情况的方式，和处理 stdin 被关掉一样——那是 abort，不是 deny，所以整个
// 回合结束，而不是拒掉一条命令。
func (s *shellSession) gateAsk() (string, bool) {
	return s.app.Ask("[y/n/a/q] ")
}

func (s *shellSession) onExit(w io.Writer) {
	s.trace.close()
	s.mu.Lock()
	last := s.a.lastPrompt
	s.mu.Unlock()
	// 总计要打在真正的屏幕上，所以渲染器为这一次调用被指回去。继续对着输出区
	// 的话，这笔账会打进一块再没人会读的缓冲里。
	s.view.out = w
	s.view.SessionSummary(last)
}

func (s *shellSession) banner() []string {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	s.mu.Unlock()

	out := []string{
		"  stage 11 · malformed — one boundary between a model's output and exec.Command",
		"",
		fmt.Sprintf("  cwd       %s", wd),
		fmt.Sprintf("  shell     %s", a.cfg.shell),
	}
	if ok, _ := s.ready(); ok {
		out = append(out,
			fmt.Sprintf("  provider  %s (%s)", pname, pcfg.Protocol),
			fmt.Sprintf("  model     %s", pcfg.Model))
	} else {
		out = append(out,
			"",
			"  No provider is configured, so nothing can be sent yet. Two ways out:",
			"",
			"    /provider-url https://your-endpoint/v1",
			"    /provider-protocol openai",
			"    /provider-model your-model-id",
			"    /provider-apikey <key>",
			"",
			"  or quit, run `set -a && . ./.env && set +a`, and start again. The",
			"  commands above save to a file outside this repo; the .env route does not.")
	}

	if s.storeErr != nil {
		out = append(out, "",
			"  "+s.storeErr.Error(),
			"  The settings commands are off until that file is fixed or deleted.",
			"  Nothing was written to it: a file that cannot be read is not a file",
			"  to overwrite.")
	}

	// 在仓库根目录里跑这个 Agent，就是让模型趁你读课的时候把课改掉的办法。
	// AGENTS.md 说用 sandbox/，而双击打开的二进制是从 .exe 恰好待着的地方起
	// 的——对任何拿 `go build -o agent .` 编出来的人来说，那正好是最不该待的
	// 地方。
	if isRepoRoot(wd) {
		out = append(out, "",
			"  This is the repo root, and the agent runs what the model says.",
			"  Use /open sandbox — or any scratch directory — before asking for anything.")
	}
	return append(out, "", "  /help lists the commands · /keys the keyboard · /status everything else", "")
}

// isRepoRoot 是个启发式判断，而且允许它是：误报的代价，是多出一行没人需要的
// 建议。
func isRepoRoot(dir string) bool {
	for _, name := range []string{"AGENTS.md", "go.mod", "docs"} {
		if _, err := os.Stat(dir + string(os.PathSeparator) + name); err != nil {
			return false
		}
	}
	return true
}

// ready 报的是眼下到底能不能发出一条提示词。
//
// 少了供应商在启动时不再致命，全部理由就在这里。一个还没画出任何东西就退掉
// 的二进制，从文件管理器里打开时就是这样：一个窗口闪几微秒，然后什么都没有
// ——而用户无从知道为什么，更别提去修。
func (s *shellSession) ready() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.a.lad == nil {
		return false, "no provider is configured yet — /provider-url, /provider-protocol, /provider-model and /provider-apikey set one up, and /status shows what is missing"
	}
	return true, ""
}

func (s *shellSession) submit(ctx context.Context, line string) error {
	s.mu.Lock()
	a := s.a
	s.msgs = append(s.msgs, userTurn(line, volatileContext(a.cfg.shell, time.Now())))
	msgs := s.msgs
	s.mu.Unlock()

	s.bus.Emit(Event{Kind: KindUserMessage, Text: line})
	out := a.runTurn(ctx, msgs)

	s.mu.Lock()
	s.msgs = out
	s.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 状态栏
// ---------------------------------------------------------------------------

func (s *shellSession) segments() []tui.Segment {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	msgs := len(s.msgs)
	s.mu.Unlock()

	who := tui.Segment{Value: "no provider", Tone: tui.ToneBad}
	if a.lad != nil {
		who = tui.Segment{Value: pname + " (" + pcfg.Protocol + ")", Tone: tui.ToneAccent}
	}
	out := []tui.Segment{who, {Value: shortModel(pcfg.Model)}}
	if seg, ok := contextSegment(s.view.lastUsage.Prompt(), s.view.window); ok {
		out = append(out, seg)
	}
	if n := s.view.session.Prompt() + s.view.session.Output; n > 0 {
		out = append(out, tui.Segment{Value: thousands(n) + " tok", Tone: tui.ToneMuted})
	}
	if s.view.prices.known() {
		out = append(out, tui.Segment{Value: fmt.Sprintf("$%.4f", s.view.sessionCost), Tone: tui.ToneMuted})
	}
	// 只有真有消息了，消息数才值得占一个字段。在一条挤不下就得丢字段的栏上，
	// "0 msg" 是那种什么都没回答的字段。
	if msgs > 0 {
		out = append(out, tui.Segment{Value: fmt.Sprintf("%d msg", msgs), Tone: tui.ToneMuted})
	}
	if p := s.trace.path(); p != "" {
		out = append(out, tui.Segment{Value: "rec", Tone: tui.ToneWarn})
	}
	if a.cfg.yolo {
		// 在一条挤不下就得丢字段的栏上，它值得占个位，也值得是上面最扎眼的那
		// 个：它是唯一一个后果为"命令不问就跑"的设置。
		out = append(out, tui.Segment{Value: "yolo", Tone: tui.ToneBad})
	}
	// 目录放在最后，因为它是最长的那个字段，也是唯一一个长度没有上限的字段；而
	// 这条栏是从第一个挤不下的字段起，往后一并丢掉。放在中间时，凡是比这条路径
	// 窄的终端，它后面的字段就全都看不见了——包括 yolo，而在这条栏上，少掉 yolo
	// 的代价是最大的。
	out = append(out, tui.Segment{Label: "in", Value: shortDir(wd), Tone: tui.ToneMuted})
	return out
}

// contextSegment 报的是上下文有多满；报不出来的时候，它一个字都不说。
//
// 有两种报不出来。第一次调用之前没有 prompt 可量，而一个"0%"读起来像是个答
// 案，而不像是"没有答案"。再就是没配窗口时没有分母——一个双击二进制起来的会
// 话正是这个状态，因为窗口是供应商的一项属性，而这时根本没有 providers.json
// 可以从里面读到它。/provider-window 就是为这个准备的，而空着的这个字段是它
// 顺手补上的最小的一件：面板自己那行上下文会退回成一个光秃秃的 token 数，没
// 有东西可以拿来除；压缩的水位线是窗口的一个比例，而窗口不存在，于是它永远
// 也到不了。
//
// 把它放在这里而不是放进 /status，图的就是那点颜色：满到该压缩了，是那种你
// 希望不用特意去找就能注意到的事。
func contextSegment(prompt, window int) (tui.Segment, bool) {
	if prompt <= 0 || window <= 0 {
		return tui.Segment{}, false
	}
	pct := float64(prompt) * 100 / float64(window)
	tone := tui.ToneGood
	switch {
	case pct >= 85:
		tone = tui.ToneBad
	case pct >= 60:
		tone = tui.ToneWarn
	}
	return tui.Segment{Label: "ctx", Value: fmt.Sprintf("%.0f%%", pct), Tone: tone}, true
}

func shortModel(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 && i+1 < len(m) {
		return m[i+1:]
	}
	return m
}

func shortDir(d string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(d, home) {
		return "~" + d[len(home):]
	}
	return d
}

func thousands(n int) string {
	switch {
	case n < 10_000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	}
}

// ---------------------------------------------------------------------------
// /status
// ---------------------------------------------------------------------------

func (s *shellSession) status() []tui.Section {
	s.mu.Lock()
	a, pname, pcfg, wd := s.a, s.pname, s.pcfg, s.wd
	s.mu.Unlock()
	v := s.view

	prov := []Row{
		{Name: "name", Value: pname},
		{Name: "protocol", Value: pcfg.Protocol},
		{Name: "model", Value: pcfg.Model},
		{Name: "base url", Value: pcfg.BaseURL},
		{Name: "api key", Value: settings.Redact(pcfg.APIKeyEnv, os.Getenv(pcfg.APIKeyEnv)), Note: "from " + pcfg.APIKeyEnv},
		{Name: "window", Value: tokensOrUnknown(pcfg.Window)},
	}
	if a.lad != nil {
		if names := a.lad.names(); len(names) > 1 {
			prov = append(prov, Row{Name: "fallback", Value: strings.Join(names, " → ")})
		}
	} else {
		prov = append(prov, Row{Name: "state", Value: "not built", Note: "nothing can be sent"})
	}

	spend := []Row{
		{Name: "calls", Value: strconv.Itoa(v.calls)},
		{Name: "commands", Value: strconv.Itoa(v.commands)},
		{Name: "tokens in", Value: fmt.Sprintf("%d + %d cache-write + %d cache-read",
			v.session.Input, v.session.CacheWrite, v.session.CacheRead)},
		{Name: "tokens out", Value: strconv.Itoa(v.session.Output)},
		{Name: "cost", Value: costOrUnknown(v.prices.known(), v.sessionCost)},
		{Name: "retries", Value: strconv.Itoa(v.retries)},
		{Name: "invalid tool calls", Value: strconv.Itoa(v.invalid)},
	}

	work := []Row{
		{Name: "directory", Value: wd},
		{Name: "shell", Value: a.cfg.shell},
		{Name: "trace", Value: orNone(s.trace.path())},
		{Name: "gate", Value: gateMode(a.g)},
		{Name: "memory", Value: yesOrNo(!s.opts.noMemory), Note: "AGENTS.md and MEMORY.md, read at startup and on /open"},
		{Name: "skills", Value: yesOrNo(!s.opts.noSkills)},
	}

	return []tui.Section{
		{Title: "provider", Rows: prov},
		{Title: "conversation", Rows: s.conversationRows()},
		{Title: "session", Rows: spend},
		{Title: "workspace", Rows: work},
		{Title: "limits", Rows: s.limitRows()},
	}
}

// conversationRows 既是 /context 打出来的东西，也是 /status 中间那一段显示
// 的东西。写成一个函数，是因为两份会各自漂走——而这里的数字，恰好就是人们拿
// 这两处互相对照的那些。
func (s *shellSession) conversationRows() []Row {
	s.mu.Lock()
	a, msgs := s.a, s.msgs
	s.mu.Unlock()

	base := len(a.system()) + toolChars()
	out := []Row{
		{Name: "messages", Value: strconv.Itoa(len(msgs))},
		{Name: "history", Value: fmt.Sprintf("%d chars", convChars(msgs))},
		{Name: "system + tools", Value: fmt.Sprintf("%d chars", base)},
		{Name: "estimated prompt", Value: fmt.Sprintf("~%d tok", a.comp.estimate(msgs, base)),
			Note: fmt.Sprintf("at %.2f chars/token from %d samples", a.comp.est.ratio, a.comp.est.obs)},
		{Name: "last billed", Value: tokensOrUnknown(a.lastPrompt)},
		{Name: "compactions", Value: strconv.Itoa(s.view.compactions)},
	}
	if problem := validConversation(msgs); problem != "" {
		// 一段畸形的对话是被下一次请求拒掉的，不是被造出它的那一次，所以像这
		// 样的报告，是它在失败之前唯一露得出脸的地方。
		out = append(out, Row{Name: "MALFORMED", Value: problem})
	}
	return out
}

func (s *shellSession) limitRows() []Row {
	var out []Row
	for _, k := range s.knobs() {
		out = append(out, Row{Name: k.name, Value: k.get(), Note: k.help})
	}
	return out
}

func tokensOrUnknown(n int) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d tok", n)
}

func costOrUnknown(known bool, v float64) string {
	if !known {
		// 编出来的零比没有数字更糟，因为它就是人们会拿去引的那个数。跟仪表盘
		// 那边同一条规矩。
		return "unknown — no prices configured"
	}
	return fmt.Sprintf("$%.4f", v)
}

func orNone(s string) string {
	if s == "" {
		return "not recording"
	}
	return s
}

func yesOrNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOrOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func gateMode(g *gate) string {
	switch {
	case g.yolo:
		return "off — every command runs unasked (--yolo)"
	case g.always:
		return "off for this session — you answered 'a'"
	default:
		return "asking"
	}
}

// ---------------------------------------------------------------------------
// 重建
// ---------------------------------------------------------------------------

// open 把这场会话指到另一个目录上。
//
// 有四样东西依赖工作目录，而这四样必须一起挪：命令在哪里跑、记忆从哪里读
// 写、技能在哪里建索引、系统提示词说目录是哪个。挪三样比一样都不挪更糟——一
// 个被告知自己在某个目录、命令却在另一个目录里跑的 Agent，会对一棵它看不见
// 的树给出自信而错误的答案。
func (s *shellSession) open(dir string) (string, error) {
	if err := os.Chdir(dir); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.a
	s.wd = dir
	a.memoryDir = dir
	sys, stable := s.assemble(a.cfg.shell, dir)
	a.system, a.stable = sys, stable

	// 对话是故意留着的。这段历史记的是干过的活，因为目录换了就把它删掉，等于
	// 把用户换目录的理由一并扔了。另一种意图有另一条命令，就是 /new，两条都
	// 有才是重点。
	msg := "now working in " + dir
	if len(s.msgs) > 0 {
		msg += fmt.Sprintf(" · %d messages kept, and they still refer to the old directory — /new starts over", len(s.msgs))
	}
	return msg, nil
}

// assemble 拼出系统提示词。调用方持有 mu。
//
// 在外壳需要跑它第二遍之前，它在 main() 里就是一条直线。它读的每样东西都是
// 磁盘上的文件，而这恰恰就是它不能只算一次的原因：/open 会换掉那些文件是哪
// 些。
func (s *shellSession) assemble(shell, wd string) (func() string, string) {
	memory := ""
	if !s.opts.noMemory {
		memory, _ = loadMemory(wd, s.bus)
	}
	var skills []skill
	if !s.opts.noSkills {
		skills = loadSkills(wd)
	}
	if len(skills) > 0 {
		idx, bodies := skillsCost(skills)
		s.bus.Emit(Event{Kind: KindSkillsIndexed, Bytes: idx, TokensBefore: bodies,
			Text: fmt.Sprintf("%d skills", len(skills))})
	}
	stable := stableContext(shell, wd) + memoryPrompt
	if memory != "" {
		stable += para + memory
	}
	stable += skillsPrompt(skills)
	full := basePrompt + para + stable
	if s.opts.breakCache {
		return func() string {
			return "Current time: " + time.Now().Format(time.RFC3339Nano) + "\n\n" + full
		}, stable
	}
	return func() string { return full }, stable
}

// reconfigure 在某个设置改过之后重建供应商。
//
// 允许它失败，也允许它把失败说出来，而不回滚任何东西。刚敲进去的那个值已经
// 存下了；要是因为端点没应答就把一个已存的设置退回去，那两步走的情形——先设
// URL，再设 key——就永远走不通了。
func (s *shellSession) reconfigure() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuildProvider()
}

// rebuildProvider 拿当前的环境把供应商重新解析一遍。
// 调用方持有 mu。
func (s *shellSession) rebuildProvider() (string, error) {
	pcfg, pname, err := s.pf.resolve(s.opts.provider)
	if err != nil {
		return "", err
	}
	if s.opts.window > 0 {
		pcfg.Window = s.opts.window
	}
	p, err := pcfg.build(s.opts.cacheBP)
	if err != nil {
		return "", err
	}
	lad, err := buildLadder(s.pf, pname, pcfg, p, s.opts.fallback, s.opts.cacheBP)
	if err != nil {
		return "", err
	}
	s.pcfg, s.pname, s.a.lad = pcfg, pname, lad

	// 价格和窗口属于供应商，所以它们跟着它一起挪。一块还在按上一个端点计价的
	// 仪表盘，给出的是一笔错账，而屏幕上没有任何东西承认它错了。
	s.view.prices = prices{in: pcfg.Prices.In, out: pcfg.Prices.Out,
		cacheRead: pcfg.Prices.CacheRead, cacheWrite: pcfg.Prices.CacheWrite}
	s.view.window = pcfg.Window
	s.a.comp.window = pcfg.Window

	_, _, info := lad.pos()
	s.bus.Emit(Event{Kind: KindProvider, Provider: &info})
	out := fmt.Sprintf("provider %s (%s) · model %s", pname, pcfg.Protocol, pcfg.Model)
	if pcfg.Window <= 0 {
		out += " · no window configured, so compaction can never fire"
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 命令
// ---------------------------------------------------------------------------

func (s *shellSession) commands() []tui.Command {
	return []tui.Command{
		{
			Name: "/compact", Group: "session",
			Help: "summarise the conversation now, without waiting for the watermark",
			Run: func(ctx context.Context, _ string, w io.Writer) error {
				s.mu.Lock()
				a, msgs := s.a, s.msgs
				s.mu.Unlock()
				base := len(a.system()) + toolChars()
				cut, why := a.comp.plan(msgs, base)
				if cut < 0 {
					return fmt.Errorf("%s", why)
				}
				out, err := a.comp.run(ctx, a.prov(), a.pol, a.httpc, a.bus, msgs, cut, base, a.dl)
				if err != nil {
					return fmt.Errorf("compaction failed: %w — the conversation is unchanged", err)
				}
				s.mu.Lock()
				s.msgs = out
				s.mu.Unlock()
				return nil
			},
		},
		{
			Name: "/context", Group: "session",
			Help: "what the conversation currently costs",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				for _, l := range tui.RenderRows(s.conversationRows(), s.app.Width()) {
					fmt.Fprintln(w, l)
				}
				return nil
			},
		},
		{
			Name: "/new", Group: "session",
			Help: "forget the conversation and start over; settings are kept",
			Run: func(_ context.Context, _ string, w io.Writer) error {
				s.mu.Lock()
				n := len(s.msgs)
				s.msgs = nil
				s.mu.Unlock()
				fmt.Fprintf(w, "  %d messages dropped\n", n)
				return nil
			},
		},
		{
			Name: "/remember", Args: "<note>", Group: "session",
			Help: "append a line to " + memoryFileForWriting,
			Run: func(_ context.Context, arg string, w io.Writer) error {
				if arg == "" {
					return fmt.Errorf("/remember needs something to remember")
				}
				s.mu.Lock()
				dir := s.a.memoryDir
				s.mu.Unlock()
				if err := remember(dir, arg); err != nil {
					return err
				}
				fmt.Fprintf(w, "  noted in %s — it takes effect next session, not this one\n", memoryFileForWriting)
				return nil
			},
		},
		{
			Name: "/trace", Args: "[path|off]", Group: "session",
			Help: "start or stop writing a JSONL event trace",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				switch arg {
				case "":
					fmt.Fprintf(w, "  %s\n", orNone(s.trace.path()))
				case "off":
					s.trace.close()
					fmt.Fprintln(w, "  not recording")
				default:
					if err := s.trace.open(arg); err != nil {
						return err
					}
					// 供应商事件在这里重发一遍，好让新文件开头就带上每个读者最
					// 先要的那件事：谁在应答，按什么价。少了它，一份从会话中途
					// 起头的 trace 里会有一堆没法还原的开销。
					s.mu.Lock()
					lad := s.a.lad
					s.mu.Unlock()
					if lad != nil {
						_, _, info := lad.pos()
						s.bus.Emit(Event{Kind: KindProvider, Provider: &info})
					}
					fmt.Fprintf(w, "  recording to %s\n", s.trace.path())
					// 从哪里读回去。没人打开的 trace 就只是个文件，而打开它的
					// 那条命令，恰恰是刚把录制打开的读者还不知道的那一条。
					fmt.Fprintf(w, "  read it back with --composer %s, which needs no key\n", s.trace.path())
				}
				return nil
			},
		},
		{
			Name: "/provider", Args: "[name]", Group: "provider",
			Help: "list the providers file, or switch to one of its entries",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				if arg == "" {
					if len(s.pf.Providers) == 0 {
						fmt.Fprintf(w, "  no providers file — this session is configured from the environment\n")
						return nil
					}
					names := providerNames(s.pf)
					sort.Strings(names)
					for _, n := range names {
						mark := " "
						if n == s.pname {
							mark = "*"
						}
						p := s.pf.Providers[n]
						fmt.Fprintf(w, "  %s %-16s %-10s %s\n", mark, n, p.Protocol, p.Model)
					}
					return nil
				}
				if _, ok := s.pf.Providers[arg]; !ok {
					return fmt.Errorf("no provider named %q (have: %s)", arg, strings.Join(providerNames(s.pf), ", "))
				}
				was := s.opts.provider
				s.opts.provider = arg
				msg, err := s.rebuildProvider()
				if err != nil {
					// 放回去。跟 /provider-url 不一样，这条命令没在磁盘上改任
					// 何东西，所以把会话留在一个建不起来的供应商上，纯粹是亏。
					s.opts.provider = was
					return err
				}
				fmt.Fprintf(w, "  %s\n", msg)
				return nil
			},
		},
		{
			Name: "/provider-window", Args: "<tokens>", Group: "provider",
			Help: "set and save how large this model's context window is",
			Run: func(_ context.Context, arg string, w io.Writer) error {
				arg = strings.TrimSpace(arg)
				if arg == "" {
					s.mu.Lock()
					have := s.pcfg.Window
					s.mu.Unlock()
					if have <= 0 {
						return fmt.Errorf("no window is configured; pass a size in tokens, e.g. /provider-window 131072")
					}
					fmt.Fprintf(w, "  %d tokens\n", have)
					return nil
				}
				n, err := strconv.Atoi(arg)
				if err != nil {
					return fmt.Errorf("want a number of tokens: %w", err)
				}
				if n < 1024 {
					// 这不是口味问题。压缩的水位线是这个数的一个比例，而一个比
					// 一份系统提示词还小的窗口，会把水位线压到地板底下：会话还
					// 一句话没说就要去压缩，而且每次都失败。
					return fmt.Errorf("want at least 1024 tokens, got %d", n)
				}
				if s.store == nil {
					return fmt.Errorf("no settings file, so there is nowhere to save this: %v", s.storeErr)
				}
				s.store.Set(envWindow, arg)
				if err := s.store.Save(); err != nil {
					return err
				}
				// 存下来之外还导出到环境里，因为 rebuildProvider 是从环境重新
				// 解析的，一个只存在于磁盘上的值要到下次启动才会生效。
				os.Setenv(envWindow, arg)

				s.mu.Lock()
				s.opts.window = n
				msg, err := s.rebuildProvider()
				s.mu.Unlock()
				fmt.Fprintf(w, "  saved to %s\n", s.store.Path())
				if err != nil {
					// 存是存下了，但供应商没建起来。这里报的是重建失败，不是这
					// 条命令失败：那个数字已经在磁盘上，下次启动就会用上。
					return err
				}
				fmt.Fprintf(w, "  %s\n", msg)
				return nil
			},
		},
		{
			Name: "/set", Args: "[name [value]]", Group: "agent",
			Help: "show or change a limit without restarting",
			Run:  s.runSet,
		},
	}
}

// envWindow 是 /provider-window 把上下文窗口存进去的地方。
//
// 它不放进 providers.json，理由和 API key 不放进去的一样：那个文件是要提交
// 的，而一场通过外壳配起来的会话在里面根本没有属于自己的条目可写。它也没做
// 成一个 --window flag，因为 flag 得记住、还得每次重敲，而供应商那几条命令
// 存在的全部意义，就是照顾一个被人双击打开的二进制。
const envWindow = "AGENT_WINDOW"

// savedWindow 从环境里把窗口读出来——到调用它的时候，一个存下来的设置已经被
// 导出到那里了。
//
// 值不合法就忽略，而不是报出来。这段跑在启动过程中，那时还没有界面可以报给
// 它；忽略的后果是一个空字段，而 /provider-window 会解释它——另一边则是一个
// 被双击打开的二进制直接致命报错，而整个外壳存在的意义，就是不让那种失败发
// 生。
func savedWindow() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(envWindow)))
	if err != nil || n < 1024 {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// /set
// ---------------------------------------------------------------------------

// knob 是一项能在运行时改的设置。
//
// 写成一张表，而不是一条命令一个，因为它们有十六个，而且形状都一样：读一个
// 数，写一个数。**不**在这里的，是启动之后就改不动的那些——它找到的 shell、
// trace 的格式——而 /set 表态的方式是不把它们列出来，不是收下改动然后不理。
type knob struct {
	name string
	help string
	get  func() string
	set  func(string) error
}

func (s *shellSession) knobs() []knob {
	a := s.a
	num := func(p *int, min int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			if n < min {
				return fmt.Errorf("want at least %d", min)
			}
			*p = n
			return nil
		}
	}
	dur := func(p *time.Duration) func(string) error {
		return func(v string) error {
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			if d <= 0 {
				return fmt.Errorf("want a positive duration")
			}
			*p = d
			return nil
		}
	}
	frac := func(p *float64) func(string) error {
		return func(v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}
			if f < 0 || f > 1 {
				return fmt.Errorf("want a fraction between 0 and 1")
			}
			*p = f
			return nil
		}
	}
	return []knob{
		{"max-turns", "tool-call rounds per user message", func() string { return strconv.Itoa(a.cfg.maxTurns) }, num(&a.cfg.maxTurns, 1)},
		{"sub-turns", "rounds a subagent gets", func() string { return strconv.Itoa(a.cfg.subTurns) }, num(&a.cfg.subTurns, 1)},
		{"max-tokens", "output budget per call", func() string { return strconv.Itoa(a.cfg.maxTokens) }, num(&a.cfg.maxTokens, 1)},
		{"max-output", "bytes of command output the model may see", func() string { return strconv.Itoa(a.cfg.maxOutput) }, num(&a.cfg.maxOutput, 1)},
		{"max-depth", "how deep subagents may nest", func() string { return strconv.Itoa(a.maxDepth) }, num(&a.maxDepth, 0)},
		{"timeout", "kill a command after this long", func() string { return a.cfg.timeout.String() }, dur(&a.cfg.timeout)},
		{"connect-timeout", "response headers must arrive within this", func() string { return a.dl.connect.String() }, dur(&a.dl.connect)},
		{"stall-timeout", "longest tolerated gap between bytes", func() string { return a.dl.idle.String() }, dur(&a.dl.idle)},
		{"call-timeout", "backstop on one whole model call", func() string { return a.dl.total.String() }, dur(&a.dl.total)},
		{"retry", "attempts per provider on a retryable failure", func() string { return strconv.Itoa(a.pol.attempts) }, num(&a.pol.attempts, 1)},
		{"retry-budget", "total time one call may wait between attempts", func() string { return a.pol.budget.String() }, dur(&a.pol.budget)},
		{"window", "context window in tokens; /provider-window also saves it", func() string { return strconv.Itoa(s.view.window) }, func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return err
			}
			if n < 1024 {
				return fmt.Errorf("want at least 1024 tokens")
			}
			// 同一个数字的三份拷贝；之所以是三份，是因为它们是在三个不同的时刻
			// 从供应商那里读来的。只设其中一份，正是仪表盘和压缩对"什么时候该
			// 触发"各说各话的由来。
			s.opts.window, s.view.window, a.comp.window = n, n, n
			return nil
		}},
		{"compact-at", "compact past this fraction of the window", func() string { return fmt.Sprintf("%.2f", a.comp.threshold) }, frac(&a.comp.threshold)},
		{"keep", "fraction of the window left in place after compacting", func() string { return fmt.Sprintf("%.2f", a.comp.keepRatio) }, frac(&a.comp.keepRatio)},
		{"yolo", "run every command without asking", func() string { return onOrOff(a.cfg.yolo) }, func(v string) error {
			b, err := parseOnOff(v, a.cfg.yolo)
			if err != nil {
				return err
			}
			a.cfg.yolo, a.g.yolo = b, b
			return nil
		}},
		{"show-request", "print the full request body before each call", func() string { return onOrOff(s.view.showRequest) }, func(v string) error {
			b, err := parseOnOff(v, s.view.showRequest)
			if err != nil {
				return err
			}
			s.view.showRequest = b
			return nil
		}},
	}
}

func parseOnOff(v string, cur bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "yes", "true", "1":
		return true, nil
	case "off", "no", "false", "0":
		return false, nil
	case "":
		return !cur, nil
	default:
		return cur, fmt.Errorf("want on or off, got %q", v)
	}
}

func (s *shellSession) runSet(_ context.Context, arg string, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := s.knobs()

	name, value := arg, ""
	if i := strings.IndexAny(arg, " \t"); i >= 0 {
		name, value = arg[:i], strings.TrimSpace(arg[i+1:])
	}
	if name == "" {
		width := 0
		for _, k := range ks {
			if len(k.name) > width {
				width = len(k.name)
			}
		}
		for _, k := range ks {
			fmt.Fprintf(w, "  %-*s  %-12s  %s\n", width, k.name, k.get(), k.help)
		}
		fmt.Fprintf(w, "\n  /set <name> <value>. Everything not listed is fixed when the process starts.\n")
		return nil
	}
	for _, k := range ks {
		if k.name != name {
			continue
		}
		if value == "" {
			fmt.Fprintf(w, "  %s = %s   %s\n", k.name, k.get(), k.help)
			return nil
		}
		if err := k.set(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(w, "  %s = %s\n", k.name, k.get())
		return nil
	}
	return fmt.Errorf("no setting called %q — /set with no argument lists them", name)
}

// Row 就是 tui.Row，换成这个文件其余部分用的那个名字。取别名，省得每一处
// 用到的地方都要 import 加限定名。
type Row = tui.Row
