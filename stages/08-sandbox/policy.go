// 阶段 08 —— 为什么你不能通过读命令来保护一个 shell。
//
// 这个文件是同一条规则的三种实现，一个比一个更好，而三个都留着的意义
// 在于：前两个正是谁都会直接拿去上线的东西，两个都在一行代码内就被击
// 败了。
//
// 这个规则，故意这么小所以它能完全被推理：
//
//	**Agent 不得读取 .env**
//
// 不是"Agent 不得做危险的事" —— 一个那么模糊的规则无法被测试，一个你
// 无法测试的策略是一个你在猜测的策略。一个文件，一个动词。
//
//	inspectString   看命令文本            被引用击败
//	inspectAST      解析它看单词          被展开击败
//	sandbox.exec    **做** shell 看 argv      看 shell.go
//
// 这个进展不是"增加更多模式"。每个级别把检查移到一个更多真理可用的
// 地方，最后一个把它移到真理完整的唯一地方：在展开之后，当参数向量是
// 最终的，而没有什么剩下可以用来躲在后面了。
package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// secretName 是这个策略保护的文件。
const secretName = ".env"

// refusal 是一个被阻止的操作和理由，用模型将读到的词。一个说"拒绝"的策略
// 只教模型再尝试；一个说它反对什么的策略让模型用另一种方式做任务。
type refusal struct {
	Level string // 哪个检查器抓住它
	What  string // 匹配的确切文本或参数
	Why   string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("blocked by the %s policy: %s (matched %q)", r.Level, r.Why, r.What)
}

// isSecretPath 报告一个路径是否指向受保护的文件。
//
// 基础名称匹配，限制是刻意的，也值得说清楚：这不解析 symlink，不规范化
// `..`，不知道 `/proc/self/cwd/.env` 是同一个文件。一个真实的策略需要
// filepath.EvalSymlinks 和一个针对根目录的包含检查 —— 即使那样也还是有
// 竞态，因为一个路径能在检查和打开之间被替换。TOCTOU 不是沙箱中的边界
// 情况，它是标准攻击。
func isSecretPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	return filepath.Base(filepath.Clean(p)) == secretName
}

// ---------------------------------------------------------------------------
// 级别 1：看这个字符串
// ---------------------------------------------------------------------------

// denyPattern 就是一个最初版本的实现总会长成的样子。
var denyPattern = regexp.MustCompile(`\.env\b`)

// inspectString 是几乎每个 Agent 宿主都会以某种形式上线的检查。
//
// 它对你想到的命令有效。它被 shell 自己的语法击败 —— 不是靠什么聪明的手
// 段，只是靠这个字符串所写的语言里那些稀松平常的特性。`cat ".e""nv"` 对
// bash 是同一个命令，对这个函数却是不同的字符串，模式再怎么完善也修不好
// 这一点，因为模式看的是源文本，而 shell 看的是源文本**意味着**什么。
//
// 测过的完整列表见 bypass_test.go。
func inspectString(command string) *refusal {
	if m := denyPattern.FindString(command); m != "" {
		return &refusal{Level: "string", What: m, Why: "the command mentions " + secretName}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 级别 2：解析它
// ---------------------------------------------------------------------------

// inspectAST 解析命令，检查一个真实的 shell 解析器产生出来的词。
//
// 这是一个真正的改进，不是装饰性的。解析器知道 `".e""nv"` 是一个单词，
// 其字面部分连接起来是 `.env`；知道 `'.env'` 是一个带引号的字面值；也
// 知道 `cat<.env` 即使没有空格也带着一个重定向。每个击败级别 1 的引用
// 技巧，到这里都会失效，因为解析器做的事和 shell 做的一样。
//
// 它没法知道的，是任何还不存在的值。`$X`、`$(...)`、`${x:-...}` 和
// `eval` 全都表示"这个值要晚一点才算出来"，而解析的那一刻，"后面"这件
// 事还没发生。这不是实现上的欠缺，而是这门语言本身的属性：shell 不是一
// 种语法，它是一个求值器，而求值器输入的解析树，并不能告诉你这个求值
// 器将会做什么。
//
// 一个解析错误被当作一个拒绝而不是一个通过。一个这无法解析的命令是一
// 个这无法判断的命令，对于一个整个工作就是"做判断"的检查来说，"我看
// 不懂，所以我放行"是错误的默认选择。
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
			// 字符串检查会完全错过、argv 检查也会错过的那一个：`cat < .env` 运行
			// `cat` **没有参数**。文件由 shell 打开，不是程序，所以一个只看 argv
			// 的策略，压根看不到文件名。
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

// 只有当整个单词都是字面值时 —— 仅当如此 —— literalWord 才会返回这个单
// 词的值。
//
// 第二个返回值是诚实的部分。一个包含参数展开或命令替换的单词，在解析
// 时没有值，而 literalWord 会如实说明这一点，不会去返回它碰巧看懂的那
// 部分。对 `.en$X` 返回 `".en"`，会比什么都不返回还糟糕，因为调用者接
// 下来会拿这个不完整的值去跟策略比较，然后认定它是安全的。
func literalWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				// '...' 是 C 风格的转义：\x2eenv' 是 .env，解析器存储的是转义文本，不
				// 是解码后的字节。这里不对它解码 —— 那样就是在策略里面重新实现一部分
				// shell，而这正是这一章通篇要讲的陷阱。报告"不是字面"，让级别 3 去看
				// 真实的值。
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false // 引号中的一个展开
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false // ParamExp、CmdSubst、ArithmExp、ExtGlob、…
		}
	}
	return b.String(), true
}
