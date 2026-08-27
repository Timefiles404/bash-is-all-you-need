// 阶段 06——composer，输入这一半。
//
// 字节进来，按键事件出去。活儿就这么点，而它比听起来难，原因只有一
// 个：终端键盘不是设备，是*协议*，而这份协议是四十年的沉积层——
// VT100、VT220、rxvt、xterm，以及所有抄过它们其中之一的东西。没有注
// 册表，也没有版本协商。两台终端的 Home 键发出的字节不一样；*同一
// 台*终端的 Up 方向键发出什么字节，还要看应用自己在这次会话里先前
// 打开了哪个模式。
//
// 所以这个文件是个解析器，解析的是一份没人写下来过的协议；它的形状
// 也由此而定：认得宽松，吃得精确，绝不猜。
//
// 下面处处成立、每个调用方都指着它的两条不变量：
//
//	进展。只要 ok 为 true，n > 0。解码器要是能返回"我解出了东西，
//	长度是零字节"，读循环就会活锁；而且它就是会在生产环境里发作，
//	就在你从没测过的那一条序列上。
//
//	诚实。ok=false 的意思是"这些字节是某个东西的前缀，我需要更
//	多"，绝不是"我不认识这个"。认不出但格式完好的序列，会被整条
//	吃掉，报成带 Raw 的 keyUnknown。把这两件事搞反，就是同一个
//	活锁换了顶帽子：调用方会永远读下去，等一条其实早就结束了的
//	序列。
package main

import (
	"bytes"
	"fmt"
	"strconv"
	"unicode/utf8"
)

type keyKind int

const (
	keyRune keyKind = iota // 可打印字符，放在 key.Rune 里
	keyEnter
	keyEsc
	keyTab
	keyShiftTab
	keyBackspace
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyPageUp
	keyPageDown
	keyDelete
	keyCtrlC
	keyCtrlD
	keyCtrlL
	keyMouse // 细节在 key.Mouse 里
	keyPaste // 括号粘贴模式的 payload，放在 key.Text 里

	// keyUnknown 的意思是"吃掉了、但不会照它行动的字节"：格式完好而我们
	// 选择不去解释的序列（功能键、设备属性应答），或者根本没看懂的输
	// 入。Raw 里永远是那几个原字节——有它，事情是一份 bug 报告；没它，
	// 就只是一桩悬案。
	keyUnknown
)

type mouseEvent struct {
	Button int  // 0 左键，1 中键，2 右键，64 滚轮上，65 滚轮下
	X, Y   int  // 从 1 开始的列/行，终端怎么报就是什么
	Press  bool // true = 按下，false = 松开
}

type key struct {
	Kind  keyKind
	Rune  rune       // keyRune
	Text  string     // keyPaste
	Mouse mouseEvent // keyMouse
	Ctrl  bool       // 终端报上来的修饰键（例如 \x1b[1;5A）
	Alt   bool
	Shift bool
	Raw   string // 吃掉的那几个原字节，给调试和 keyUnknown 用
}

// ---------------------------------------------------------------------------
// Escape 键的歧义
//
// 这就是为什么这里有两个入口而不是一个。它值得好好搞明白，因为这
// 不是这个解码器身上长的疙瘩。这是终端协议里的一个洞，至今没有任
// 何一个解码器补上过它。
//
// Escape 是 0x1b。它同时也是每个方向键、每个功能键、每份鼠标报告、
// 每次粘贴的第一个字节。所以当一次读返回的缓冲区以一个光秃秃的
// 0x1b 结尾时，恰好只有两种可能：
//
//  1. 用户按了 Escape，故事到此为止；
//  2. 用户按了 Up 键，而 tty 只把 "\x1b[A" 的第一个字节交给了我们，
//     因为这次读恰好落在了那串字节的中间。
//
// 字节完全一样。没有长度前缀，没有终止符，没有标志位——流里没有任
// 何东西能区分这两种情况，将来也不会有，因为这套编码当初是为那样一
// 台终端设计的：在那台终端上，"Escape"和"转义序列引导符"本来就是故
// 意做成同一个键的。
//
// 唯一能把两者分开的信号是**时间**。终端发出的序列是一整串一起到达的：
// 剩下的字节早就躺在 pty 缓冲区里，晚几微秒而已。而人按下 Escape，
// 跟他接下来打的任何东西之间，会留下几十毫秒的空档。所以地球上每个
// 终端程序都用同一种办法收场——读一次，看见一个孤零零的 ESC，等一
// 小段超时，什么都没来，就当它是 Escape。
//
// 那段超时，就是 Escape 在 vim 里、tmux 里、你 shell 的 vi 模式里、
// 你用过的每个 TUI 里都慢半拍的原因。这不是代码慢，也不是 bug：是程
// 序在拒绝猜。也正因为它，`set -sg escape-time 10` 出现在 GitHub 上
// 一半的 tmux 配置里；也正因为它，把这个值调到 0 之后，方向键和 Alt
// 组合键就开始在 ssh 上误触发——跨过真实的网络，那串字节再也没法保
// 证落在一次读里。
//
// 这个文件做的决定，是把那条策略挡在解码器**外面**：
//
//	decodeKey       —— "ok=false"：我需要更多字节，我不猜。
//	decodeKeyFinal  —— "调用方已经等过了；孤零零的 ESC 就是 Escape。"
//
// 两个理由。第一是正确性：合适的超时不是字节流的性质，是链路的性
// 质。25 毫秒在本地 pty 上很宽裕，在一条跑满的 ssh 会话上远远不够，
// 所以这个数字属于知道链路情况的那一层，而不属于解析器——解析器根
// 本看不见链路。
//
// 第二条更现实：解码器一旦有了自己的时钟，就不再可测了。"过了 50 毫秒"
// 这种事没法写成表驱动测试，你只能写 sleep；而写满 sleep 的测试套件，
// 迟早没人再跑。解码器只要是输入的纯函数，一毫秒就能吃下一万条字节
// 序列——这才是 keys_test.go 底部那张表付得起的唯一原因。
//
// 调用方那一半契约只有四行：
//
//	k, n, ok := decodeKey(buf)
//	if !ok {
//		// 带截止时间的短读；等它到点而什么都没来：
//		k, n, ok = decodeKeyFinal(buf)
//	}
//
// ---------------------------------------------------------------------------

// decodeKey 从 buf 的前端解出一个按键。
// 返回按键、吃掉了多少字节；当 buf 里只有某个序列的开头、调用方必须
// 再读时，返回 ok=false。
func decodeKey(buf []byte) (key, int, bool) { return decodeOne(buf, false) }

// decodeKeyFinal 是 decodeKey 在另一种情形下的样子：调用方已经等过
// 了，不会再有字节来了。decodeKey 解不开的那个歧义，由它来解。
//
// 只要缓冲区非空，它就一定有进展，只有一个例外：没有终止符的括号粘
// 贴，它仍然返回 ok=false。这个例外为什么是对的选择、而不是抽象漏了
// 底，见 decodePaste。
func decodeKeyFinal(buf []byte) (key, int, bool) { return decodeOne(buf, true) }

func decodeOne(buf []byte, final bool) (key, int, bool) {
	if len(buf) == 0 {
		return key{}, 0, false
	}
	switch {
	case buf[0] == 0x1b:
		return decodeEscape(buf, final)
	case buf[0] < 0x20 || buf[0] == 0x7f:
		return decodeControl(buf), 1, true
	default:
		return decodeRune(buf, final)
	}
}

// ---------------------------------------------------------------------------
// 普通字节
// ---------------------------------------------------------------------------

func decodeRune(buf []byte, final bool) (key, int, bool) {
	// utf8.DecodeRune 分不清"非法"和"还没读完"：两种情况它都返回
	// (RuneError, 1)。能把两者分开的是 FullRune；省掉它，界面就会在有人
	// 打出跨越读边界的 emoji 时，每次都显示一个替换字符。字节本来是好
	// 的，是解码器看得太早，然后又吐出 U+FFFD 把证据毁了。
	if !utf8.FullRune(buf) {
		if !final {
			return key{}, 0, false
		}
		// 调用方等过了，后面始终没来，所以这真的是一个被截断的 rune——
		// 字符中间断了线，或者某个程序往 tty 上只写了半个字符串。把这
		// 个碎片吃掉，让循环能往前走，但不要编出一个用户根本没打过的
		// 字符。
		return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		// 永远不可能作为 rune 开头的字节：0xFF，或者我们早先丢掉的那个
		// 碎片留下的游离续接字节。正好跳过一个字节——手动重新同步就是
		// 恢复一条流的办法；而在这里吐 U+FFFD，会把一个字形塞进用户的
		// 文档里。
		return key{Kind: keyUnknown, Raw: string(buf[:1])}, 1, true
	}
	return key{Kind: keyRune, Rune: r, Raw: string(buf[:size])}, size, true
}

func decodeControl(buf []byte) key {
	b := buf[0]
	k := key{Raw: string(buf[:1])}

	// 这里的顺序是有讲究的，它编码的是一个决定，不是一次查表。Ctrl-M
	// 和 Enter 是同一个字节。Ctrl-I 和 Tab、Ctrl-J 和换行、Ctrl-H 和
	// Backspace 也一样。终端分不出来，我们也分不出来，所以有名字的那个
	// 键赢——每个编辑器、shell 和 TUI 都是这么干的，也正因如此，从来没
	// 有哪个应用把 Ctrl-M 绑到 Enter 之外的东西上还能落得好。
	switch b {
	case 0x0d, 0x0a: // CR（原始模式从不转换它）和 LF
		k.Kind = keyEnter
	case 0x09:
		k.Kind = keyTab
	case 0x7f, 0x08:
		// 两个都算，一直都算。现代终端为 Backspace 发的是 0x7f（DEL），
		// 而 terminfo 条目里声称它发的是 0x08（BS）；你实际拿到哪个，取
		// 决于模拟器、stty erase，还有你是不是在 tmux 里面。只把其中一
		// 个当 Backspace，就会招来那句经典的 bug 报告："我的退格键在
		// ssh 上打出 ^H"。
		k.Kind = keyBackspace
	case 0x03:
		k.Kind = keyCtrlC
	case 0x04:
		k.Kind = keyCtrlD
	case 0x0c:
		k.Kind = keyCtrlL
	case 0x00:
		// 终端为 Ctrl-Space 发的是 NUL，历史上叫 Ctrl-@。把它报成一个带
		// Ctrl 的空格，才是应用真正拿来做绑定的形态。
		k.Kind, k.Rune, k.Ctrl = keyRune, ' ', true
	default:
		switch {
		case b >= 0x01 && b <= 0x1a:
			// Ctrl-A..Ctrl-Z。用小写，因为 Ctrl-A 和 Ctrl-Shift-A 是同一个
			// 字节，加了 Shift 的形态在线上根本不存在。
			k.Kind, k.Rune, k.Ctrl = keyRune, rune('a'+b-1), true
		case b >= 0x1c && b <= 0x1f:
			// Ctrl-\ Ctrl-] Ctrl-^ Ctrl-_——Z 之上的四个控制符。字节就是
			// 对应的 ASCII 字符减去 0x40。
			k.Kind, k.Rune, k.Ctrl = keyRune, rune(b+0x40), true
		default:
			// 到不了这里：0x20 以下只剩 0x1b 这一个字节，而它在轮到我们之
			// 前，就被 decodeOne 分给 decodeEscape 了。
			k.Kind = keyUnknown
		}
	}
	return k
}

// ---------------------------------------------------------------------------
// 转义序列
// ---------------------------------------------------------------------------

func decodeEscape(buf []byte, final bool) (key, int, bool) {
	if len(buf) == 1 {
		// 重头戏。见 decodeKey 上面那段注释。
		if !final {
			return key{}, 0, false
		}
		return key{Kind: keyEsc, Raw: "\x1b"}, 1, true
	}

	switch buf[1] {
	case '[':
		return decodeCSI(buf, final)
	case 'O':
		return decodeSS3(buf, final)
	}

	// ESC 后面跟别的任何东西，都算 Alt+那个键，因为终端把 Alt 实现成了
	// "metaSendsEscape"：按住 Alt，就得到一个 ESC 前缀。这也就意味着
	// Alt-a 和"先 Escape，再 a"同样是字节相同的，上面那套关于时间的说法
	// 在这里照样成立。我们无条件选 Alt，理由和每个编辑器一样：在一次读
	// 之内，Alt 是压倒性更可能的意图；而真正需要区分的调用方，手里有我
	// 们没有的时间信息。
	//
	// 递归不是偷懒。Alt-Enter、Alt-Backspace，甚至 ESC ESC [ A（Alt-Up，
	// tmux 会转发它）都白捡着对了，而这些序列里的每一条，最后都会有人
	// 来报 bug。
	k, n, ok := decodeOne(buf[1:], final)
	if !ok {
		return key{}, 0, false
	}
	k.Alt = true
	k.Raw = string(buf[:n+1])
	return k, n + 1, true
}

// incompleteEscape 回答的是"这肯定是一条转义序列的开头，而且它肯定
// 还没结束"。
func incompleteEscape(buf []byte, final bool) (key, int, bool) {
	if !final {
		return key{}, 0, false
	}
	// 调用方等过了，剩下的始终没到。我们知道它开了一条序列（`[` 或者
	// `O` 就在那儿摆着），也知道它没结束，所以没有按键可报——但我们仍
	// 然必须把它吃掉，否则调用方会永远问同一个问题。keyUnknown 带着
	// Raw 字节；把它们打到日志里，你通常会发现某台终端在干一件文档里没
	// 写的事，而这值得知道。
	return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
}

// decodeSS3 处理的是"SS3"引导符，ESC O。
//
// 它的存在是因为 DECCKM，也就是"应用光标键"——应用自己打开的一个模
// 式，打开之后方向键到达的形式是 ESC O A，而不是 ESC [ A。解码器要
// 是只认 CSI 那一种形式，在你的笔记本上看着完美，然后有人把它跑在
// tmux 里、screen 里，或者跑在一个把这个模式开着没关的 readline 程
// 序底下，它就当场坏掉。bug 报告写的是"方向键打出一个字母"，而那个
// 字母就在这儿。
func decodeSS3(buf []byte, final bool) (key, int, bool) {
	if len(buf) < 3 {
		return incompleteEscape(buf, final)
	}
	k := key{Raw: string(buf[:3])}
	switch buf[2] {
	case 'A':
		k.Kind = keyUp
	case 'B':
		k.Kind = keyDown
	case 'C':
		k.Kind = keyRight
	case 'D':
		k.Kind = keyLeft
	case 'H':
		k.Kind = keyHome
	case 'F':
		k.Kind = keyEnd
	default:
		// ESC O P/Q/R/S 是 F1-F4，这个范围里剩下的是应用模式下的数字小
		// 键盘。吃掉，报上去，不解释。
		k.Kind = keyUnknown
	}
	return k, 3, true
}

// pasteEnd 是括号粘贴模式的终止符。
var pasteEnd = []byte("\x1b[201~")

// decodeCSI 解析一条 CSI 序列：ESC [ ，然后是参数字节，然后是中间字
// 节，然后是正好一个终结字节。
//
// 这些字节范围来自 ECMA-48，也正是它们让这个解码器能吃下一条从没见
// 过的序列。这件事比认全每个键更重要：终端会主动发来没有任何键盘产
// 生过的 CSI 应答（光标位置、设备属性、焦点进出），而对它们唯一安全
// 的处理就是整条咽掉。去猜长度，或者用"我需要更多字节"退场，都会把
// 一份路过的状态报告变成一个卡死的界面。
func decodeCSI(buf []byte, final bool) (key, int, bool) {
	p := 2
	for p < len(buf) && buf[p] >= 0x30 && buf[p] <= 0x3f { // 参数 0-9 : ; < = > ?
		p++
	}
	q := p
	for q < len(buf) && buf[q] >= 0x20 && buf[q] <= 0x2f { // 中间字节，空格到 /
		q++
	}
	if q >= len(buf) {
		return incompleteEscape(buf, final)
	}

	fb := buf[q]
	if fb < 0x40 || fb > 0x7e {
		// 不属于任何 CSI 类别的字节——实际情况里，是同一个 tty 的另一个
		// 写入方在序列中间插进来的控制字符。ECMA-48 规定控制字符照样执
		// 行，序列就此作废，所以我们把残骸吃到那个捣乱的字节为止，但
		// **不**包括它，把它留给下一次调用。Ctrl-C 落在一份鼠标报告中间
		// 的时候，也必须照样管用。
		return key{Kind: keyUnknown, Raw: string(buf[:q])}, q, true
	}

	n := q + 1
	params := buf[2:p]
	raw := string(buf[:n])

	if fb == '~' && bytes.Equal(params, []byte("200")) {
		return decodePaste(buf, n)
	}
	if (fb == 'M' || fb == 'm') && len(params) > 0 && params[0] == '<' {
		return decodeMouse(params[1:], fb == 'M', raw, n)
	}
	if fb == 'M' && len(params) == 0 {
		// 老式的 X10 鼠标上报（模式 1000，没有 1006）：CSI M 后面跟三个
		// **原始**字节——button+32、column+32、row+32——它们根本不属于
		// 这条 CSI 序列。我们把它们咽掉，什么也不报。
		//
		// 这是故意不支持，不是只是没实现。一个坐标就是一个字节加上 32
		// 的偏置，所以 X10 能表达的最大列号是 255-32 = 223；再往后，它要
		// 么截住要么绕回，看模拟器而定，于是在宽窗口右半边点一下，上报
		// 的格子就悄悄错了。SGR（1006）之所以存在，恰恰是因为那套编码没
		// 法兼容地修好，所以我们要 SGR，也只解 SGR。
		//
		// 但那三个字节我们还是得吃掉：它们是流里普普通通的字节，放它们
		// 掉进 decodeRune，就等于往用户正在编辑的东西里打进三个乱码字
		// 符。
		if len(buf) < n+3 {
			return incompleteEscape(buf, final)
		}
		return key{Kind: keyUnknown, Raw: string(buf[:n+3])}, n + 3, true
	}

	ps, ok := csiParams(params)
	if !ok {
		// 读不成数字的参数——比如设备属性应答 \x1b[?1;2c 这类私有模式的
		// 回复。格式完好，但不是按键。
		return key{Kind: keyUnknown, Raw: raw}, n, true
	}

	k := key{Raw: raw}
	switch fb {
	case 'A':
		k.Kind = keyUp
	case 'B':
		k.Kind = keyDown
	case 'C':
		k.Kind = keyRight
	case 'D':
		k.Kind = keyLeft

	// Home 和 End 是键盘上最糟的两个键，下面那八种形态就是原因。四条互
	// 不相干的血脉，至今全都还在野外跑着：
	//
	//	\x1b[1~ \x1b[4~   VT220 的编号，那会儿这两个键还叫 Find 和
	//	                  Select。Linux 控制台发的就是这个。
	//	\x1b[7~ \x1b[8~   rxvt 把它们重新编了号，凡是从 rxvt 传下来
	//	                  的东西都沿用了这套新编号。
	//	\x1b[H  \x1b[F    xterm 自己的形态，普通光标模式下。
	//	\x1bOH  \x1bOF    同样这两个键，一旦 DECCKM 打开——而 tmux
	//	                  和 screen 会替你打开它，一声不响。
	//
	// 这些没有一个会被废弃，因为废掉任何一个，就会弄坏某台还有人在用的
	// 终端。八种全收下，代价是六行；只收六种，代价是一份写着"我从 Mac
	// 上 ssh 进来，Home 键没反应"的 bug 报告。
	case 'H':
		k.Kind = keyHome
	case 'F':
		k.Kind = keyEnd

	case 'Z':
		// CSI Z 是 CBT，"反向制表"——Shift-Tab 发的就是它。Shift 标志保
		// 持 false：那个标志留给终端用参数报上来的修饰键，而这条序列一
		// 个参数都没有。shift 的信息在 Kind 里，调用方可以对它 switch。
		k.Kind = keyShiftTab
		return k, n, true

	case '~':
		switch csiParam(ps, 0, 0) {
		case 1, 7:
			k.Kind = keyHome
		case 4, 8:
			k.Kind = keyEnd
		case 3:
			k.Kind = keyDelete
		case 5:
			k.Kind = keyPageUp
		case 6:
			k.Kind = keyPageDown
		default:
			// 2 是 Insert；11-15 和 17-24 是 F1-F12；201 是没有开头的粘贴终
			// 止符，意思是我们丢了同步——与其咽掉它，不如说出来。全都吃
			// 掉，一个也不解释。
			k.Kind = keyUnknown
			return k, n, true
		}

	default:
		k.Kind = keyUnknown
		return k, n, true
	}

	// 修饰键永远是**第二个**参数，字母终结（\x1b[1;5A）和波浪号终结
	// （\x1b[3;5~）都一样。字母终结的第一个参数，在终端把 CSI 当输出
	// *发出来*的时候是重复次数；作为输入它永远是 1，忽略它是对的。
	applyModifier(&k, csiParam(ps, 1, 1))
	return k, n, true
}

// decodePaste 处理 \x1b[200~ … \x1b[201~（括号粘贴模式，模式 2004）。
//
// payload 被逐字节抄出来，绝不再解码一遍。这就是这个模式的全部意
// 义：粘贴进来的文本里家常便饭地带着 0x1b、0x0d 和控制字节，解码器
// 要是把它们再送回自己手里，一段粘进来的 shell 脚本就会变成一串方向
// 键，一条粘进来的多行提交信息会变成若干次 Enter——够把它的一半提交
// 出去。括号粘贴模式之所以存在，是因为当年有人在 vim 的插入模式里粘
// 过一次东西。
func decodePaste(buf []byte, start int) (key, int, bool) {
	i := bytes.Index(buf[start:], pasteEnd)
	if i < 0 {
		// 没有终止符——注意它连从 decodeKeyFinal 进来也返回 ok=false，
		// 这是那条规则唯一被破的地方。
		//
		// 破得是故意的。转义超时回答的是关于人的手指的问题；而粘贴是机
		// 器在按管道速度写，一兆字节的粘贴会分很多次读到达，中间的空档
		// 什么也说明不了。"超时到了"根本不能算粘贴已经结束的证据，所以
		// 在这里把它定下来，结果就是先把一半 payload 当文本吐出去，再把
		// 剩下的当一个个按键解码——包括里面任何一个换行，而在 prompt 界
		// 面里，换行意味着把一行没写完的东西提交出去。调用方想要一道安
		// 全阀，就自己给缓冲区设个上限，把长得离谱又没有终止符的粘贴丢
		// 掉。那是一条策略，而策略住在调用方那边，跟时钟一样。
		return key{}, 0, false
	}
	n := start + i + len(pasteEnd)
	return key{
		Kind: keyPaste,
		Text: string(buf[start : start+i]),
		Raw:  string(buf[:n]),
	}, n, true
	// 值得知道一件事：这个协议没有转义机制，所以 payload 里要是真带着
	// "\x1b[201~"，粘贴就提前结束了。这个洞在协议里，不在这儿——终端
	// 的糊法是在发送之前先把粘贴文本里的 ESC 滤掉，这也正是你没法把一
	// 条转义序列粘进终端让它执行的原因。
}

// ---------------------------------------------------------------------------
// 鼠标
// ---------------------------------------------------------------------------

// 修饰键位和移动位，都塞在 SGR 的 button 字段里。
const (
	mouseShift  = 0x04
	mouseAlt    = 0x08
	mouseCtrl   = 0x10
	mouseMotion = 0x20 // 拖动/移动的报告里会置上
)

// decodeMouse 解析 SGR（1006）鼠标报告：按下是 \x1b[<b;x;yM，松开是
// \x1b[<b;x;ym。只认 SGR——见 decodeCSI 里关于 X10 的那段话。
func decodeMouse(params []byte, press bool, raw string, n int) (key, int, bool) {
	ps, ok := csiParams(params)
	if !ok || len(ps) < 3 || ps[0] < 0 || ps[1] < 0 || ps[2] < 0 {
		return key{Kind: keyUnknown, Raw: raw}, n, true
	}
	b, x, y := ps[0], ps[1], ps[2]

	k := key{Kind: keyMouse, Raw: raw}
	k.Shift = b&mouseShift != 0
	k.Alt = b&mouseAlt != 0
	k.Ctrl = b&mouseCtrl != 0
	k.Mouse = mouseEvent{
		// 把修饰键位和移动位剥掉，让 Button 就是个按钮编号，别的什么都
		// 不是。省掉这一步，Ctrl-点击就会报成 button 16，按住左键拖动会
		// 报成 button 32，而每一处对 Button 做的 switch，都会多出一个
		// `default:`，悄悄把真的点击吃掉。滚轮位（0x40）**不**剥，因为在
		// 这条线上，滚轮上就真的是 button 64——编码就是这么命名的。
		Button: b &^ (mouseShift | mouseAlt | mouseCtrl | mouseMotion),
		X:      x,
		Y:      y,
		// 移动报告的终结字节是 'M'，所以一次拖动露出来的样子，是在新格
		// 子上按了一下。这是故意的：mouseEvent 没有 Drag 字段，调用方靠
		// "中间没有松开的一串按下"把拖动拼回来——反正它为了知道哪个键
		// 按着，本来就得跟着这些。而只有调用方要了模式 1002/1003，移动
		// 报告才会来。
		Press: press,
	}
	return k, n, true
}

// ---------------------------------------------------------------------------
// 参数
// ---------------------------------------------------------------------------

// csiParams 把 CSI 参数字节切成整数，或者报告它们根本不是数字。省略
// 掉的参数变成 -1 而不是 0，因为 ECMA-48 说省略的参数意思是"用默认
// 值"，而默认值并不总是零——\x1b[;5A 是 Ctrl-Up，不是 Ctrl-什么零。
func csiParams(p []byte) ([]int, bool) {
	if len(p) == 0 {
		return nil, true
	}
	out := make([]int, 0, 4)
	for _, f := range bytes.Split(p, []byte{';'}) {
		// xterm 的 modifyOtherKeys 和 kitty 的协议会把子参数塞在冒号后面
		// （\x1b[1;5:3A 用来区分按下和重复）。这里没有任何地方要用它
		// 们，而丢掉它们好过把整条序列拒掉：基础键和它的修饰键仍然读得
		// 清清楚楚。终端选择用上更丰富的协议，不该换来一台方向键不好使
		// 的终端。
		if i := bytes.IndexByte(f, ':'); i >= 0 {
			f = f[:i]
		}
		if len(f) == 0 {
			out = append(out, -1)
			continue
		}
		v, err := strconv.Atoi(string(f))
		if err != nil {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

func csiParam(ps []int, i, def int) int {
	if i >= len(ps) || ps[i] < 0 {
		return def
	}
	return ps[i]
}

// applyModifier 解码 xterm 的修饰键参数。
//
// 这套编码是位掩码**加一**，而这个 +1 是终端代码里最可靠的一个
// off-by-one。没有修饰键是 1，不是 0，所以你得先减再掩。忘了它，每
// 个修饰键都会读成挨着的下一个：Ctrl-Up（5）过来时带着 shift，
// Shift-Right（2）过来变成 Alt；而由此来的 bug 之所以让人抓狂，恰恰
// 是因为方向键还"能用"——只是带着错的修饰键在用，在某一台终端上，
// 有时候。
func applyModifier(k *key, mod int) {
	if mod <= 1 {
		return
	}
	m := mod - 1
	k.Shift = m&1 != 0
	k.Alt = m&2 != 0
	k.Ctrl = m&4 != 0
	// 第 8 位是 Meta。忽略：这十年的终端没有一个会发它，而把一个幽灵修
	// 饰键折进 Alt，会让 Alt 变得不可信。
}

// ---------------------------------------------------------------------------
// 调试用的渲染。它存在，是为了让失败的测试说"wanted keyUp, got
// keyUnknown raw=\"\\x1bOA\""，而不是"wanted 6, got 20"——这就是修一
// 下和搭进去一个下午的区别。
// ---------------------------------------------------------------------------

var keyKindNames = [...]string{
	keyRune:      "keyRune",
	keyEnter:     "keyEnter",
	keyEsc:       "keyEsc",
	keyTab:       "keyTab",
	keyShiftTab:  "keyShiftTab",
	keyBackspace: "keyBackspace",
	keyUp:        "keyUp",
	keyDown:      "keyDown",
	keyLeft:      "keyLeft",
	keyRight:     "keyRight",
	keyHome:      "keyHome",
	keyEnd:       "keyEnd",
	keyPageUp:    "keyPageUp",
	keyPageDown:  "keyPageDown",
	keyDelete:    "keyDelete",
	keyCtrlC:     "keyCtrlC",
	keyCtrlD:     "keyCtrlD",
	keyCtrlL:     "keyCtrlL",
	keyMouse:     "keyMouse",
	keyPaste:     "keyPaste",
	keyUnknown:   "keyUnknown",
}

func (k keyKind) String() string {
	if k >= 0 && int(k) < len(keyKindNames) {
		return keyKindNames[k]
	}
	return fmt.Sprintf("keyKind(%d)", int(k))
}

func (k key) String() string {
	s := k.Kind.String()
	switch k.Kind {
	case keyRune:
		s += fmt.Sprintf("(%q)", k.Rune)
	case keyPaste:
		s += fmt.Sprintf("(%q)", k.Text)
	case keyMouse:
		s += fmt.Sprintf("(button %d at %d,%d press=%v)", k.Mouse.Button, k.Mouse.X, k.Mouse.Y, k.Mouse.Press)
	}
	for _, m := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "ctrl"}, {k.Alt, "alt"}, {k.Shift, "shift"}} {
		if m.on {
			s += "+" + m.name
		}
	}
	return s + fmt.Sprintf(" raw=%q", k.Raw)
}
