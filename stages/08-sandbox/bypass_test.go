package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这一章的证据是生成出来的，不是凭记忆断言的。
//
// 一个规则 —— "不要读 .env" —— 和每个命令一个问题：绕过真的有效吗，三个
// 检查器中哪一个抓住它？
//
// 每个用例都要**先验证确实是一个真实的绕过**：把策略关掉后运行一遍，确
// 认文件内容真的被读了出来。一张满是从未真正奏效过的命令的绕过表，什么
// 都证明不了，而这正是这类表格最常出错的地方。

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

// runIn 在一个拿着金丝雀文件的临时目录中执行一个命令，报告秘密是否逃逸和
// 沙箱是否阻挡了什么。
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

// TestTheBypassTable 就是这一章。它打印它测到的表。
func TestTheBypassTable(t *testing.T) {
	dir := bypassDir(t)

	// 如果环境不能运行基线，其他的都没有任何意义。大声跳过而不是报告一个零表。
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

		// 1. 绕过真的在什么都挡不住的情况下有效吗？
		r.works, _, r.outputWhenNotEnforcing = runIn(t, dir, c.command, false)

		// 2. 两个静态检查器。
		r.l1 = inspectString(c.command) != nil
		r.l2 = inspectAST(c.command) != nil

		// 3. 解释器，执行策略。
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

	// --- 表必须满足的断言 -------------------------

	var stringMissed, astMissed int
	for _, r := range rows {
		if !r.works {
			// 一个不读文件的用例不是绕过，不应该在表中假装是一个。
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

	// 两个静态检查都必须失败——而且，比这张表本来要证明的东西更有趣的
	// 是——它们必须在**不同的命令**上失败。
	//
	// 字符串检查错过 `cat ".e""nv"` 因为文本被分割了。AST 检查抓住那个，错过
	// `eval "cat .env"`，文本就在那里但这个词属于一个还不存在的程序。两个集
	// 合互不包含。
	//
	// 这就堵死了看完这一章后最容易冒出来的想法 —— "那就两个检查都跑一遍" ——
	// 因为攻击者不需要同时打败两个条件。每个命令一次只需要击败一个属性，挑
	// 哪一个，shell 语法本身就留了空间。TestExpansionBeatsParsing 就是同时
	// 击败两者的那一行代码。
	if stringMissed == 0 {
		t.Error("the regexp check caught every bypass, which would mean shell quoting does not exist; the table is wrong")
	}
	if astMissed == 0 {
		t.Error("the AST check caught everything, which would mean expansion is decidable at parse time. It is not; the table is wrong")
	}
	stringOnly, astOnly := 0, 0
	for _, r := range rows {
		if !r.l1 && r.l2 {
			astOnly++ // 解析抓住了模式匹配错过的
		}
		if r.l1 && !r.l2 {
			stringOnly++ // 模式匹配抓住了解析错过的
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

// 这一章里最用力倚靠的这一对，被单独断言了一遍，这样一旦失败，报出
// 来的是机制的名字，而不是表里的第几行。
func TestExpansionBeatsParsing(t *testing.T) {
	// 文件名在文本里压根没有出现（所以字符串检查无从匹配），而这个词是
	// `${X}v`——一个粘在字面值上的参数展开（所以 AST 检查正确给出"我现在
	// 还不知道这个"）。一行代码，两个静态检查全都被击败，文件照样被读了出
	// 来。
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

// 重定向这个用例专门有自己的测试，因为它是那个能在**每个级别**都击败
// argv-only 策略的用例，包括沙箱 —— 除非沙箱也处理文件打开。`cat < .env`
// 运行 cat 时完全不带参数。
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

// 还有这条诚实的边界，是用断言证明的，不只是嘴上承认一下。这个测试会
// 在沙箱未能拦住命令时**通过**，这正是重点所在：解释器看得到每一个命
// 令，却看不进任何一个的内部。
func TestTheSandboxCannotSeeInsideAProgram(t *testing.T) {
	dir := bypassDir(t)

	// 任何一个把程序当参数接收的解释器都行，文件名是在那个程序参数**内
	// 部**拼出来的，所以它永远不会出现在 argv 里。沙箱看到的是 `awk -v
	// a=.en <a program>`，它对程序里的内容没有意见——有意见就等于要重新实
	// 现一遍 awk。
	//
	// 候选者按照"在 Git Bash 旁边能找到"的可能性大小排好了顺序。选中的那
	// 个必须验证真的能用 —— 在 Windows 上，exec.LookPath("python") 高兴地
	// 就找到一个会打印微软商店广告的 App Execution Alias 存根，用它搭起来
	// 的测试会报出一个假阴性。
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
