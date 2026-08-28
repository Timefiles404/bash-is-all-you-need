package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这一章的证据，是跑出来的，不是凭记忆断言的。
//
// 一条规则——"不许读 .env"——每条命令一个问题：这次绕过真的成了吗，
// 三个检查器里哪一个抓住了它？
//
// 每个案例都*先验证过它是真的绕过*：把策略关掉跑一遍，确认文件内容
// 真的出来了。一张全是从来没成功过的命令的绕过表，什么也证明不了，
// 而这正是这类表格最常见的错法。

const canary = "CANARY_dc41f0b7"

// bypassCase 是一次读取受保护文件的尝试。
type bypassCase struct {
	name    string
	command string
	note    string
}

var bypassCases = []bypassCase{
	{"plain", `cat .env`, "the command everybody blocks"},
	{"single quotes", `cat '.env'`, ""},
	{"split across quotes", `cat ".e""nv"`, "one word to the shell, two strings to a regexp"},
	{"empty quotes inside", `cat .en''v`, ""},
	{"backslash", `cat .en\v`, ""},
	{"leading ./", `cat ./.env`, "a different string, the same file"},
	{"ANSI-C quoting", `cat $'\x2eenv'`, "the name never appears as text at all"},
	{"variable", `X=.env; cat $X`, "the value does not exist until runtime"},
	{"command substitution", `cat $(echo .env)`, ""},
	{"eval", `eval "cat .env"`, "the program is data until it is not"},
	{"parameter default", `cat "${MISSING:-.env}"`, ""},
	{"loop", `for f in .env; do cat "$f"; done`, ""},
	{"redirect", `cat < .env`, "argv is just [\"cat\"] — no filename anywhere in it"},
	{"nested shell", `sh -c 'cat .env'`, "a whole program smuggled in one argument"},
}

// runIn 在放着 canary 文件的临时目录里执行命令，报告秘密有没有跑出
// 去、沙箱有没有拦下什么。
func runIn(t *testing.T, dir, command string, enforce bool) (leaked, blocked bool, out string) {
	t.Helper()
	sb := newSandbox(dir, NewBus(), enforce)
	r := sb.run(command, 10*time.Second)
	out = r.Stdout + r.Stderr
	return strings.Contains(r.Stdout, canary), len(sb.blocked) > 0, out
}

func bypassDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretName), []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTheBypassTable 就是这一章。它把自己量出来的表打印出来。
func TestTheBypassTable(t *testing.T) {
	dir := bypassDir(t)

	// 环境要是连基线都跑不了，后面的全都没意义。大声跳过，而不是报一
	// 张全是零的表。
	if leaked, _, out := runIn(t, dir, "cat .env", false); !leaked {
		t.Skipf("this machine cannot run `cat` through the interpreter, so the table cannot be measured: %s", out)
	}

	type row struct {
		c                      bypassCase
		works, l1, l2, l3      bool
		outputWhenNotEnforcing string
	}
	var rows []row

	for _, c := range bypassCases {
		var r row
		r.c = c

		// 1. 什么都不拦的时候，这次绕过真的成得了吗？
		r.works, _, r.outputWhenNotEnforcing = runIn(t, dir, c.command, false)

		// 2. 两个静态检查器。
		r.l1 = inspectString(c.command) != nil
		r.l2 = inspectAST(c.command) != nil

		// 3. 解释器，开着强制。
		leaked, blocked, _ := runIn(t, dir, c.command, true)
		r.l3 = blocked && !leaked

		rows = append(rows, r)
	}

	t.Log("")
	t.Logf("%-22s %-7s %-8s %-6s %-9s", "command", "works?", "string", "ast", "sandbox")
	for _, r := range rows {
		mark := func(b bool) string {
			if b {
				return "blocked"
			}
			return "  --   "
		}
		t.Logf("%-22s %-7v %-8s %-6s %-9s  %s", r.c.name, r.works,
			mark(r.l1), mark(r.l2), mark(r.l3), r.c.note)
	}
	t.Log("")

	// --- 表格必须满足的断言 ----------------------------------------------

	var stringMissed, astMissed int
	for _, r := range rows {
		if !r.works {
			// 读不到文件的案例就不是绕过，不该待在表里冒充绕过。
			t.Errorf("%q did not actually read the file, so it is not a bypass — fix or remove the case.\noutput: %s",
				r.c.command, r.outputWhenNotEnforcing)
			continue
		}
		if !r.l1 {
			stringMissed++
		}
		if !r.l2 {
			astMissed++
		}
		if !r.l3 {
			t.Errorf("the sandbox did NOT stop %q (%s).\n"+
				"This is the claim the whole chapter rests on: after expansion there is nowhere left to hide.\n"+
				"output: %s", r.c.command, r.c.name, r.outputWhenNotEnforcing)
		}
	}

	// 两个静态检查都必须输，而且——比这张表本来要展示的结论更有意思的
	// 一点——它们必须输在**不同**的命令上。
	//
	// 字符串检查漏掉 `cat ".e""nv"`，因为文本被切开了。AST 检查抓得住这
	// 一条，却漏掉 `eval "cat .env"`：文本明明就在那儿，可那个词属于一
	// 个还不存在的程序。两个集合谁也不包含谁。
	//
	// 这就把对本章最顺口的回应——"两个检查都跑"——打死了，因为攻击者不
	// 必同时击破两条。每条命令只需要一次击破一个性质，而 shell 语法给了
	// 你挑哪一个的自由。TestExpansionBeatsParsing 就是一行同时击破两者
	// 的命令。
	if stringMissed == 0 {
		t.Error("the regexp check caught every bypass, which would mean shell quoting does not exist; the table is wrong")
	}
	if astMissed == 0 {
		t.Error("the AST check caught everything, which would mean expansion is decidable at parse time. It is not; the table is wrong")
	}
	stringOnly, astOnly := 0, 0
	for _, r := range rows {
		if !r.l1 && r.l2 {
			astOnly++ // 解析抓住了模式匹配漏掉的
		}
		if r.l1 && !r.l2 {
			stringOnly++ // 模式匹配抓住了解析漏掉的
		}
	}
	t.Logf("string missed %d · ast missed %d · caught only by ast: %d · caught only by string: %d",
		stringMissed, astMissed, astOnly, stringOnly)
	if astOnly == 0 || stringOnly == 0 {
		t.Errorf("one check's misses are a subset of the other's (ast-only %d, string-only %d) — "+
			"the chapter claims they fail on disjoint sets, and that claim is the reason "+
			"'just run both' does not work", astOnly, stringOnly)
	}
}

// 这一章倚得最重的就是这一对，单独断言出来，这样它挂了的时候报出
// 来的是机制，不是某个行号。
func TestExpansionBeatsParsing(t *testing.T) {
	// 文件名在文本里根本不出现（所以字符串检查没东西可匹配），而那个词
	// 是 `${X}v`，参数展开粘上一个字面量（所以 AST 检查很正确地报告"这
	// 个我还没法知道"）。一行，两个静态检查全破，文件被读了出来。
	const command = `X=.en; eval 'cat ${X}v'`
	if inspectString(command) != nil {
		t.Error("the string check matched a command in which the filename never appears; the case is not testing what it claims")
	}
	if r := inspectAST(command); r != nil {
		t.Errorf("the AST check claims to have resolved %q, which would require evaluating the shell at parse time", command)
	}
	dir := bypassDir(t)
	leaked, blocked, out := runIn(t, dir, command, true)
	if leaked || !blocked {
		t.Errorf("the interpreter did not stop a command it had already expanded to `cat .env` (leaked=%v blocked=%v)\n%s",
			leaked, blocked, out)
	}
}

// 重定向这一条单独有个测试，因为它在**每一层**上都能击破只看 argv
// 的策略，沙箱也不例外——除非沙箱连文件打开也管。`cat < .env` 跑起
// cat 来，压根没有任何参数。
func TestRedirectIsNotVisibleInArgv(t *testing.T) {
	sb := newSandbox(bypassDir(t), NewBus(), true)
	_ = sb.run("cat < .env", 10*time.Second)

	for _, argv := range sb.execs {
		if strings.Contains(argv, secretName) {
			t.Fatalf("argv %q contained the filename, so this case no longer demonstrates the point", argv)
		}
	}
	if len(sb.blocked) == 0 {
		t.Error("the redirect was not blocked: OpenHandler is the only thing that can see a file the shell opens itself, " +
			"and without it a policy that inspects argv has a hole shaped exactly like `<`")
	}
}

// 还有那条诚实的界限，是断言出来的，不只是嘴上承认。沙箱拦不住这条
// 命令时，这个测试**通过**，而这正是要害：解释器看得见每一条命令，
// 却看不进任何一条命令的里面。
func TestTheSandboxCannotSeeInsideAProgram(t *testing.T) {
	dir := bypassDir(t)

	// 只要是把程序当参数收的解释器就行，而文件名是在程序**内部**拼出来
	// 的，这样它永远不会出现在 argv 里。沙箱看到的是
	// `awk -v a=.en <a program>`，对程序的内容毫无意见，因为要有意见就
	// 得把 awk 实现一遍。
	//
	// 候选按它们出现在 Git Bash 旁边的可能性排序。选中的那个必须验证过
	// 真的能用——在 Windows 上，exec.LookPath("python") 会欢天喜地地找到
	// 一个 App Execution Alias 桩，那玩意儿只会打印一条 Microsoft Store
	// 的广告，而建在它上面的测试会报出假阴性。
	candidates := []struct{ name, command string }{
		{"awk", `awk -v a=.en 'BEGIN{f=a"v"; while((getline l < f)>0) print l}'`},
		{"perl", `perl -e '$f=".en"."v"; open(F,"<",$f); print <F>;'`},
		{"python3", `python3 -c "p='.en'+'v'; print(open(p).read())"`},
		{"python", `python -c "p='.en'+'v'; print(open(p).read())"`},
	}
	var command string
	for _, c := range candidates {
		if _, err := exec.LookPath(c.name); err != nil {
			continue
		}
		if leaked, _, _ := runIn(t, dir, c.command, false); leaked {
			command = c.command
			break
		}
	}
	if command == "" {
		t.Skip("no working scripting interpreter on PATH; the limit this test documents still holds")
	}

	leaked, blocked, out := runIn(t, dir, command, true)
	if blocked {
		t.Fatalf("the sandbox blocked %q — if it can now see inside an interpreter, the chapter's honest limit needs rewriting\n%s", command, out)
	}
	if !leaked {
		t.Fatalf("the command did not read the file, so it does not demonstrate the limit; output: %s", out)
	}
	t.Log("as documented: the sandbox saw one exec, allowed it, and the program did the rest. " +
		"An embedded interpreter is a policy and observability layer, not a security boundary.")
}

// 另一种极限：不是解释器有的那种，是接线曾经有过的那种。
//
// --sandbox 是一句关于整场会话的断言，而一场会话是会派活出去的。
// newChild 是一个字段一个字段地点名子 Agent 继承什么的，而只要沙箱
// 不在这份名单上，子 Agent 拿到的 sb 就是 nil，走的就是 runCommand
// 里 runBash 那条分支，派出去的活是在一个真的 shell 里跑的——没有策
// 略，没有 sandbox_exec 事件，会话最后打出的那份统计里也什么都没
// 有。
//
// 这里是穿过 runCommand 去验的，而不是拿 child.sb 和 parent.sb 比指
// 针，因为那个字段并不是要断言的东西。要断言的是：一条策略要拒的命
// 令，换成子 Agent 来跑，照样会被拒；而且这次拒绝会进到父 Agent 用
// 来汇报的那份审计记录里——这两件事，一次重构都可能弄坏，哪怕它还老
// 老实实地把指针复制过去。
func TestASubagentRunsInsideTheParentsSandbox(t *testing.T) {
	dir := bypassDir(t)

	// 这样 runCommand 的两条分支就都扎在同一个目录上，于是没进沙箱的那条
	// 读到的是真实存在的那个文件，而不是碰巧没找着、看着像通过了。
	// bypassDir 本来就用的 t.TempDir，所以测试结束时这一切都会撤掉。
	t.Chdir(dir)

	// 有真 bash 就用真 bash，好让被测的那条分支是生产环境里的那条——一个
	// 子 Agent 跑着一个真正不受限的 shell——而不是环境凑出来的假象。底下
	// 的代码其实不需要它：沙箱在 execMiddleware 里就把这条命令拒了，压根
	// 轮不到它去 PATH 上找 `cat`。
	shell, _ := findBash()

	parent, _ := mulAgent(&gate{yolo: true}, shell)
	parent.sb = newSandbox(dir, parent.bus, true)

	child := parent.newChild("read the config#1", func() string { return "child system" })
	out := child.runCommand(1, "call_1", "cat "+secretName)

	if strings.Contains(out, canary) {
		t.Errorf("the subagent read %s and the contents went into a model's context:\n%s", secretName, out)
	}
	if !strings.Contains(out, "blocked by the sandbox/exec policy") {
		t.Errorf("the sandbox did not refuse the subagent's command, so --sandbox holds for everything the parent "+
			"runs and stops at the first `task` call — the one boundary in this stage is one delegation away from "+
			"not existing.\nthe child was told:\n%s", out)
	}
	if len(parent.sb.blocked) == 0 {
		t.Error("the parent's sandbox recorded nothing, so report() describes a fraction of the session while " +
			"reading as though it described all of it: the exec, the open and the refusal counts a human is shown " +
			"at the end would cover only the commands the parent happened to run itself")
	}
}
