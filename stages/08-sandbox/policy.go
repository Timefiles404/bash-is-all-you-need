// 阶段 08——为什么读命令字符串保护不了 shell。
//
// 这个文件里是同一条规则的三种实现，一个比一个强；三个都留着，是因为前两
// 种正是所有人真在用的，而且都能被一行命令绕过。
//
// 规则故意定得极小，好让它能被彻底想清楚：
//
//	**Agent 不得读 .env**
//
// 不是"Agent 不得做危险的事"——含糊到这个程度的规则没法测，而测不了的策
// 略，就是你在猜的策略。一个文件，一个动作。
//
//	inspectString   看命令文本                 被引号绕过
//	inspectAST      解析它，看解析出的词       被展开绕过
//	sandbox.exec    **成为** shell，看 argv    见 shell.go
//
// 这三级的递进不是"再多加几条模式"。每一级都把检查搬到能看见更多真相的地
// 方，最后一级搬到了真相唯一完整的地方：展开之后——那时参数向量已经定
// 了，再没有东西可以藏在语法背后。
package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// secretName 就是策略要保护的那个文件。
const secretName = ".env"

// refusal 是一次被拦下的操作，加上理由——而且是用模型将会读到的措辞写
// 的。只说"拒绝"的策略，除了让模型再试一次之外什么也没教给它；说清自己反
// 对什么的策略，才让模型能换条路把事办完。
type refusal struct {
	Level string // 哪一级检查抓到的
	What  string // 命中的那段原文或那个参数
	Why   string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("blocked by the %s policy: %s (matched %q)", r.Level, r.Why, r.What)
}

// isSecretPath 判断一个路径指的是不是那个受保护的文件。
//
// 只比对 base name，这个局限是故意的，也值得说出来：它不解析 symlink，不
// 规范化 `..`，也不知道 `/proc/self/cwd/.env` 是同一个文件。真正的策略需
// 要 filepath.EvalSymlinks，还需要拿根目录做一次包含性检查——就算做了也
// 还是有竞态，因为检查和 open 之间，路径可以被换掉。TOCTOU 在沙箱里不是
// 边角情况，它就是标准攻击手法。
func isSecretPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	return filepath.Base(filepath.Clean(p)) == secretName
}

// ---------------------------------------------------------------------------
// 第 1 级：看字符串
// ---------------------------------------------------------------------------

// denyPattern 就是第一版实现永远长的那个样子。
var denyPattern = regexp.MustCompile(`\.env\b`)

// inspectString 是几乎每个 Agent 宿主都以某种形式带着的那道检查。
//
// 它对你想到过的命令有效。绕过它的是 shell 自己的语法——不需要什么高明
// 手段，就是那个字符串所用语言的普通特性。`cat ".e""nv"` 对 bash 来说是
// 同一条命令，对这个函数来说是另一个字符串；再怎么打磨模式也补不上，因
// 为模式看的是源文本，而 shell 看的是源文本*意味着什么*。
//
// 实测清单见 bypass_test.go。
func inspectString(command string) *refusal {
	if m := denyPattern.FindString(command); m != "" {
		return &refusal{Level: "string", What: m, Why: "the command mentions " + secretName}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 第 2 级：解析它
// ---------------------------------------------------------------------------

// inspectAST 把命令解析一遍，检查真正的 shell 解析器产出的那些词。
//
// 这是实打实的改进，不是装样子。解析器知道 `".e""nv"` 是一个词，它的字面
// 部分拼起来就是 `.env`；知道 `'.env'` 是带引号的字面量；也知道 `cat<.env`
// 里有重定向，哪怕中间没空格。凡是能干掉第 1 级的引号花招，到这里全死，
// 因为解析器干的正是 shell 干的那件事。
//
// 它没法知道的，是任何还不存在的值。`$X`、`$(...)`、`${x:-...}` 和 `eval`
// 说的都是"这个值待会儿才算出来"，而解析的时刻，待会儿还没到。这不是实现
// 上的欠缺，这是语言的性质：shell 不是一套语法，它是个求值器；而求值器输
// 入的语法树，说不出求值器会做什么。
//
// 解析失败按拒绝处理，而不是放行。这里解析不了的命令，就是它判断不了的命
// 令；而"我没看懂，所以我放它过去"，对一道整个职责就是判断的检查来说，是
// 错的默认行为。
func inspectAST(command string) *refusal {
	f, err := syntax.NewParser().Parse(strings.NewReader(command), "cmd")
	if err != nil {
		return &refusal{Level: "ast", What: command, Why: "the command could not be parsed, so it could not be checked"}
	}

	var found *refusal
	syntax.Walk(f, func(node syntax.Node) bool {
		if found != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CallExpr:
			for _, w := range n.Args {
				if lit, ok := literalWord(w); ok && isSecretPath(lit) {
					found = &refusal{Level: "ast", What: lit,
						Why: "an argument resolves to " + secretName}
					return false
				}
			}
		case *syntax.Redirect:
			// 这一种字符串检查完全看不见，argv 检查也照样看不见：
			// `cat < .env` 跑 `cat` 的时候**一个参数都没有**。文件是
			// shell 打开的，不是程序打开的，所以只看 argv 的策略，根
			// 本就没见过那个文件名。
			if n.Word != nil {
				if lit, ok := literalWord(n.Word); ok && isSecretPath(lit) {
					found = &refusal{Level: "ast", What: lit,
						Why: "a redirect targets " + secretName}
					return false
				}
			}
		}
		return true
	})
	return found
}

// literalWord 返回一个词的值——当且仅当整个词都是字面量。
//
// 第二个返回值才是诚实的那一半。含参数展开或命令替换的词，在解析时刻没
// 有值，这个函数就照实说，而不是把自己碰巧看懂的那几段返回出去。给
// `.en$X` 返回 `".en"` 比什么都不返回更糟：调用方会拿一个不完整的值去比
// 策略，然后断定它是安全的。
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				// $'...' 是 C 风格转义：$'\x2eenv' 就是 .env，而解析器
				// 存的是转义文本，不是解码后的字节。这里不去解码——那
				// 等于在策略里重新实现一小块 shell，而这正是本章要讲的
				// 那个陷阱。报"不是字面量"，让第 3 级去看真正的值。
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false // 引号里面有个展开
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false // ParamExp、CmdSubst、ArithmExp、ExtGlob……
		}
	}
	return b.String(), true
}
