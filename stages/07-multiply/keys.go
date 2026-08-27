// 第 06 阶段——composer，输入的那一半。
//
// 字节进，按键事件出。那就是整个工作，而它比听上去更难，原因恰好
// 只有一个：终端键盘不是一个设备，它是一份**协议**，而这份协议是
// 四十年的沉积物——VT100、VT220、rxvt、xterm，以及抄了它们其中
// 之一的所有东西。没有注册表，也没有版本协商。两个终端为 Home 键
// 发送的字节可以不一样，**同一个**终端为上箭头发送的字节也可以
// 不一样，取决于应用程序自己在会话早些时候打开了什么模式。
//
// 所以这个文件是一份没人写下来过的协议的解析器，它的形态正是由此
// 而来：宽容地识别，精确地消费，绝不猜测。
//
// 下面从头到尾都有两条不变量，每一个调用者都靠它们：
//
//	进度。只要 ok 为真，n 就一定大于 0。一个会返回"我解码出了点东西，
//	但它是零字节长"的解码器，会让读循环陷入**活锁**——而且专挑你
//	从没测试过的那个序列，在生产环境里发作。
//
//	诚实。ok=false 的意思是"这些字节是某样东西的前缀，我还需要更多"，
//	而绝不是"我不认识这个"。一个无法识别、但格式良好的序列，会被
//	整体消费掉，报告为 keyUnknown，并附上 Raw 字段。把这两者弄反，
//	就是同一种**活锁**换了顶帽子：调用者会永远读下去，
//	等一个早就已经结束的序列。
package main

import (
	"bytes"
	"fmt"
	"strconv"
	"unicode/utf8"
)

type keyKind int

const (
	keyRune keyKind = iota // 可打印的字符，在 key.Rune 中
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
	keyMouse // 细节在 key.Mouse 中
	keyPaste // 括号粘贴有效负载，在 key.Text 中

	// keyUnknown 是"我们消费并不会对其采取行动的字节"：
	// 一个我们选择不解释的格式良好序列（函数键、设备属性回复），
	// 或我们完全无法理解的输入。Raw 始终持有确切字节，
	// 这是神秘和 bug 报告之间的区别。
	keyUnknown
)

type mouseEvent struct {
	Button int  // 0 左，1 中，2 右，64 轮向上，65 轮向下
	X, Y   int  // 1 基列/行恰好如终端报告的那样
	Press  bool // 真 = 按，假 = 释放
}

type key struct {
	Kind  keyKind
	Rune  rune       // keyRune
	Text  string     // keyPaste
	Mouse mouseEvent // keyMouse
	Ctrl  bool       // 终端报告的修饰符（例如 \x1b[1;5A）
	Alt   bool
	Shift bool
	Raw   string // 消费的确切字节，用于调试和 keyUnknown
}

// ---------------------------------------------------------------------------
// Escape 模糊性
//
// 这就是为什么有两个入口点，而不是一个，这件事值得真正弄懂，因为它
// 不是这个解码器身上的一个疣。它是终端协议里的一个洞，没有一个
// 解码器，在任何地方，曾经把它补上过。
//
// Escape 是 0x1b。它也是每一个箭头键、每一个功能键、每一次鼠标
// 报告、每一次粘贴的第一个字节。所以当一次读取返回的缓冲区，结尾
// 是孤零零一个 0x1b 时，恰好有两种可能：
//
//  1. 用户按下了 Escape，故事到此为止；
//  2. 用户按下了上箭头，而 tty 只把 "\x1b[A" 的第一个字节递给了我们，
//     因为这次读取碰巧卡在了这次突发的两个字节中间。
//
// 字节是完全相同的。没有长度前缀，没有终止符，没有标志位——流里
// 没有任何东西能区分这两种情况，以后也不会有，因为这套编码设计
// 出来的时候，针对的就是一种"Escape"键和"转义序列引导符"故意
// 共用同一个键的终端。
//
// 唯一能把它们分开的信号是**时间**。终端自己发出的序列，是作为
// 一整个突发到达的：剩下的字节早就已经躺在 pty 缓冲区里了，只比
// 第一个字节晚几微秒。而人类按下 Escape 键，在敲下一个字符之前，
// 会留出几十毫秒的间隙。所以地球上的每一个终端程序，都用同一种
// 方式来解决这个问题——先读，看到孤零零一个 ESC，等一小段超时，
// 如果什么都没等到，就把它当成 Escape。
//
// 那个超时，就是为什么 Escape 在 vim 里、tmux 里、你 shell 的 vi
// 模式里、你用过的每一个 TUI 里，都感觉慢半拍的原因。这不是代码
// 写得慢，也不是 bug：是一个程序在拒绝猜测。这也是为什么
// `set -sg escape-time 10` 会出现在 GitHub 上一半的 tmux 配置里，
// 也是为什么把它调到 0 会让方向键和 Alt 组合键在 ssh 上开始乱
// 触发——隔着一条真实的网络，突发不再保证落在同一次读取里了。
//
// 这个文件做出的决定，是把这条策略留在解码器**之外**：
//
//	decodeKey       ——"ok=false"：我需要更多字节，我不会猜。
//	decodeKeyFinal  ——"调用者已经等过了；孤零零一个 ESC 就是 Escape。"
//
// 两个原因。第一，正确性：正确的超时时长，不是字节流本身的属性，
// 而是链路的属性。25ms 在本地 pty 上绰绰有余，放到一个繁忙的 ssh
// 会话里又太短——所以这个数字，该由懂链路的那一层来决定，而不是
// 由看不见链路的解析器来决定。
//
// 第二，也更现实的一点：解码器一旦拥有了自己的时钟，那一刻起，
// 它就不再是可测试的了。"经过了 50 毫秒"这种事，你没法写成表驱动
// 测试——你只能写一个 sleep，而一个写满 sleep 的测试套件，是人们
// 迟早会不想再运行的套件。一个相对其输入而言是纯函数的解码器，
// 一毫秒内就能吃下一万条字节序列——这也是 keys_test.go 底部
// 那张表"养得起"的唯一原因。
//
// 而调用者这一半的约定，只有四行：
//
//	k, n, ok := decodeKey(buf)
//	if !ok {
//		// 短读，带着一个截止时间；一旦它超时却什么都没等到：
//		k, n, ok = decodeKeyFinal(buf)
//	}
//
// ---------------------------------------------------------------------------

// decodeKey 从 buf 的开头解码出一个键。返回值是这个键、它消费掉的
// 字节数，以及一个 ok 标志——当 buf 里只有一段序列的开头、调用者
// 必须再读更多字节时，ok 为 false。
func decodeKey(buf []byte) (key, int, bool) { return decodeOne(buf, false) }

// decodeKeyFinal 是调用者已经等待且没有更多字节到达的情况下的
// decodeKey。它解决 decodeKey 无法解决的模糊性。
//
// 它在任何非空缓冲区上取得进展，恰好一个异常：无法终止的括号
// 粘贴，仍然返回 ok=false。看 decodePaste，了解为什么那个例外
// 是正确的选择，而不是抽象中的泄漏。
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
	// utf8.DecodeRune 无法区分"无效"和"尚未完成"：它为两者都返回
	// (RuneError, 1)。FullRune 是分离它们的调用，跳过它，后果就是：
	// 每当有人打出一个刚好卡在读取边界上的表情符号，用户界面最终
	// 显示的就是一个替换字符。字节是好的；解码器只是看得太早，
	// 然后通过发出 U+FFFD 销毁证据。
	if !utf8.FullRune(buf) {
		if !final {
			return key{}, 0, false
		}
		// 调用者等过了，可后面的字节还是没来，所以这真的是一个被截断的
		// rune——要么是连接在字符打到一半时断开了，要么是某个程序往 tty
		// 里只写了半条字符串。把这个片段消费掉，好让循环能继续往前走，
		// 但不要凭空造出一个用户从没打过的字符。
		return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		// 一个永远不可能是 rune 开头的字节：0xFF，或者是某个我们已经丢弃的
		// 片段里，残留下来的一个游离续字节。只跳过恰好一个字节——手动
		// 重新同步，就是你恢复这条流的办法；这里如果放个 U+FFFD 进去，
		// 就是把一个字形硬塞进用户的文档里。
		return key{Kind: keyUnknown, Raw: string(buf[:1])}, 1, true
	}
	return key{Kind: keyRune, Rune: r, Raw: string(buf[:size])}, size, true
}

func decodeControl(buf []byte) key {
	b := buf[0]
	k := key{Raw: string(buf[:1])}

	// 顺序在这里很重要，它编码决定而不是查找。Ctrl-M 和 Enter 是相同
	// 的字节。Ctrl-I 和 Tab、Ctrl-J 和换行、Ctrl-H 和 Backspace 也是。
	// 终端无法区分它们，我们也无法，所以命名的键赢了——这正是每个
	// 编辑器、shell 和 TUI 的做法，也正因为如此，从来没有哪个应用程序
	// 把 Ctrl-M 绑定到 Enter 以外的东西还能全身而退。
	switch b {
	case 0x0d, 0x0a: // CR（原始模式永远不翻译它）和 LF
		k.Kind = keyEnter
	case 0x09:
		k.Kind = keyTab
	case 0x7f, 0x08:
		// 两者，总是。0x7f (DEL) 是现代终端为 Backspace 发送的，而 0x08
		// (BS) 是 terminfo 条目声称它发送的；你得到哪个取决于仿真器、
		// stty 擦除，以及你是否在 tmux 内。只把其中之一当成 Backspace，
		// 就会生成经典的"我的 backspace 在 ssh 上打印 ^H"这种 bug 报告。
		k.Kind = keyBackspace
	case 0x03:
		k.Kind = keyCtrlC
	case 0x04:
		k.Kind = keyCtrlD
	case 0x0c:
		k.Kind = keyCtrlL
	case 0x00:
		// NUL 是终端为 Ctrl-Space 发送的字节，往上追溯就是 Ctrl-@。
		// 把它报告成"带 Ctrl 修饰符的空格"，这才是应用程序实际会去
		// 绑定的形式。
		k.Kind, k.Rune, k.Ctrl = keyRune, ' ', true
	default:
		switch {
		case b >= 0x01 && b <= 0x1a:
			// Ctrl-A..Ctrl-Z。小写，因为 Ctrl-A 和 Ctrl-Shift-A
			// 是相同的字节，移位形式在线上不存在。
			k.Kind, k.Rune, k.Ctrl = keyRune, rune('a'+b-1), true
		case b >= 0x1c && b <= 0x1f:
			// Ctrl-\ Ctrl-] Ctrl-^ Ctrl-_ ——四个控件在 Z 以上。
			// 字节正好是 ASCII 字符减 0x40。
			k.Kind, k.Rune, k.Ctrl = keyRune, rune(b+0x40), true
		default:
			// 无法到达：0x20 下唯一剩余的字节是 0x1b，
			// 而 decodeOne 在我们被调用之前将其路由到 decodeEscape。
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
		// 中心部分。看 decodeKey 上方的注释块。
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

	// ESC 后面跟着别的任何东西，就是 Alt+那个键，因为终端把 Alt 实现
	// 成了"metaSendsEscape"：按住 Alt，得到的就是一个 ESC 前缀。这
	// 意味着 Alt-a 和"先 Escape，再 a"在字节上也是完全相同的，上面
	// 同样的时间论证在这里一样适用。我们无条件地选择 Alt，原因跟每个
	// 编辑器一样：在一次读取以内，Alt 压倒性地更可能是用户的真实
	// 意图；而如果调用者真的需要区分这两种情况，它手里有的是我们
	// 没有的时间信息。
	//
	// 递归不是偷懒。它不花额外力气，就能把 Alt-Enter、Alt-Backspace，
	// 甚至 ESC ESC [ A（也就是 tmux 会转发的那个 Alt-Up）都处理对，
	// 而这些序列里的每一个，最后都会有人为它提交一份 bug 报告。
	k, n, ok := decodeOne(buf[1:], final)
	if !ok {
		return key{}, 0, false
	}
	k.Alt = true
	k.Raw = string(buf[:n+1])
	return k, n + 1, true
}

// incompleteEscape 回答"这肯定是转义序列的开始，
// 并且它肯定还没有完成"。
func incompleteEscape(buf []byte, final bool) (key, int, bool) {
	if !final {
		return key{}, 0, false
	}
	// 调用者等待了，其余的从未到达。我们知道它开始了一个序列（右边
	// 有一个 `[` 或 `O`）并且我们知道它从未结束，所以没有要报告的
	// 击键——但我们必须仍然消费它，或调用者永远问相同问题。keyUnknown
	// 携带的是原始字节；把它们记下来，你常常会发现是某个终端在做
	// 没有文档记载的事——这值得知道。
	return key{Kind: keyUnknown, Raw: string(buf)}, len(buf), true
}

// decodeSS3 处理的是"SS3"引导符，也就是 ESC O。
//
// 这个函数之所以存在，是因为 DECCKM，也就是"应用光标键"模式——
// 应用程序会主动打开这个模式，打开之后，方向键就会以 ESC O A
// 的形式到达，而不是 ESC [ A。一个只认得 CSI 形式的解码器，在你
// 自己的笔记本上看起来天衣无缝，可一旦有人在 tmux 里、在 screen
// 里，或者在某个把这个模式开着没关的、基于 readline 的程序下
// 运行它，它就会崩溃。bug 报告上写的是"方向键打出了一个字母"，
// 而那个字母，正是这里这一个。
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
		// ESC O P/Q/R/S 是 F1-F4，而范围的其余部分是应用模式中的
		// 数字键盘。消费、报告、不解释。
		k.Kind = keyUnknown
	}
	return k, 3, true
}

// pasteEnd 终止括号粘贴。
var pasteEnd = []byte("\x1b[201~")

// decodeCSI 解析的是一个 CSI 序列：ESC [，然后是参数字节，然后是
// 中间字节，最后恰好一个终止字节。
//
// 这些字节范围的定义来自 ECMA-48，这也是这个解码器能吞下一个它
// 从没见过的序列的全部原因。这一点，比认全每一个键还要重要：
// 终端会主动发来一些没人请求过的 CSI 回复（光标位置、设备属性、
// 焦点进出）——这些都不是键盘敲出来的，对它们唯一安全的处理
// 方式，就是原样整个吞掉。要是靠猜一个长度，或者干脆用"需要更多
// 字节"来打退堂鼓，就会把一条无关的状态报告，变成一个卡死的界面。
func decodeCSI(buf []byte, final bool) (key, int, bool) {
	p := 2
	for p < len(buf) && buf[p] >= 0x30 && buf[p] <= 0x3f { // 参数 0-9 : ; < = > ?
		p++
	}
	q := p
	for q < len(buf) && buf[q] >= 0x20 && buf[q] <= 0x2f { // 中间字节，从空格到 /
		q++
	}
	if q >= len(buf) {
		return incompleteEscape(buf, final)
	}

	fb := buf[q]
	if fb < 0x40 || fb > 0x7e {
		// 不属于任何 CSI 类别的字节——实际上，是另一个同样在往这个 tty
		// 写数据的写入者，在序列中途注入进来的一个控制字符。ECMA-48
		// 规定：控制字符照样执行，序列则被放弃，所以我们把残骸一路消费
		// 到冒犯的那个字节之前，但**不包括**它本身，留给下一次调用去
		// 处理。Ctrl-C 必须仍然能用，哪怕它恰好落在一次鼠标报告的中间。
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
		// 遗留的 X10 鼠标报告（模式 1000，不带 1006）：CSI M 后面跟着三个
		// **原始**字节——button+32、column+32、row+32——这三个字节根本
		// 不属于 CSI 序列。我们把它们吞掉，什么都不报告。
		//
		// 这是故意不支持，不是单纯没实现。坐标是用一个字节偏移 32 表示
		// 的，所以 X10 能表达的最大列号是 255-32 = 223；超过这个界限，
		// 视仿真器而定，它要么被夹死在边界上，要么直接回绕，于是在一个
		// 宽窗口右侧的点击，就会悄悄地报告出错误的格子。SGR (1006) 的
		// 存在，正是因为那种编码没法在保持兼容的前提下修复，所以我们只
		// 认 SGR，也只解码 SGR。
		//
		// 但那三个字节我们还是得吃掉：它们是这条流里普普通通的字节，
		// 要是让它们掉进 decodeRune，就会把三个垃圾字符敲进用户正在
		// 编辑的任何东西里。
		if len(buf) < n+3 {
			return incompleteEscape(buf, final)
		}
		return key{Kind: keyUnknown, Raw: string(buf[:n+3])}, n + 3, true
	}

	ps, ok := csiParams(params)
	if !ok {
		// 我们无法以数字形式读取的参数——比如私有模式回复的设备属性应答
		// \x1b[?1;2c。格式良好，但不是一个按键。
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

	// Home 和 End 是键盘上最糟的两个键，下面的八种形式
	// 解释了为什么。四个独立的系统，全都还在使用中：
	//
	//	\x1b[1~ \x1b[4~   VT220 编号，那时键被标记为
	//	                  Find 和 Select。Linux 控制台会发送这个。
	//	\x1b[7~ \x1b[8~   rxvt 重新编号了它们，所有从 rxvt 衍生的
	//	                  终端都保留了这个编号。
	//	\x1b[H  \x1b[F    xterm 自己的形式，在普通光标模式下。
	//	\x1bOH  \x1bOF    开启 DECCKM 后的同两个键——tmux 和
	//	                  screen 为你打开了它，但没有告诉你。
	//
	// 这八种形式永远不会被废弃，因为废弃其中任何一个都会
	// 破坏某个人仍在使用的终端。接受全部八种形式只需六行；
	// 接受其中六种会让你收到一个 bug 报告，内容是"当我从 Mac
	// ssh 进入时 Home 没有反应"。
	case 'H':
		k.Kind = keyHome
	case 'F':
		k.Kind = keyEnd

	case 'Z':
		// CSI Z 是 CBT，"光标向后制表"——Shift-Tab 发送的序列。Shift 标志保持为假：
		// 它是为终端以参数形式报告的修饰符预留的，而这个序列没有参数。Shift 藏在
		// Kind 里，调用者可以对它写 switch 分支来判断。
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
			// 2 是 Insert；11-15 和 17-24 是 F1-F12；201 是一个没有开启符的粘贴结束
			// 符，意味着我们失去同步，宁可说出来也不想吞掉它。全部消耗，但无一被解读。
			k.Kind = keyUnknown
			return k, n, true
		}

	default:
		k.Kind = keyUnknown
		return k, n, true
	}

	// 修饰符总是**第二个**参数，对于字母结尾
	// （\x1b[1;5A）和波浪号结尾（\x1b[3;5~）都一样。
	// 字母结尾的第一个参数是当终端**发出** CSI 作为输出时的重复计数；
	// 作为输入时总是 1，忽略它是正确的。
	applyModifier(&k, csiParam(ps, 1, 1))
	return k, n, true
}

// decodePaste 处理 \x1b[200~ … \x1b[201~（括号粘贴，模式 2004）。
//
// 有效载荷逐字复制，永远不会重新解码。这就是这个模式的整个意义：
// 粘贴的文本经常包含 0x1b、0x0d 和控制字节，
// 一个把它们重新运行一遍的解码器会把粘贴的 shell 脚本
// 变成箭头键，把粘贴的多行提交消息变成多少个
// 回车按下，取决于提交一半内容需要多少个。括号粘贴
// 存在是因为有人曾在 vim 的插入模式下粘贴过。
func decodePaste(buf []byte, start int) (key, int, bool) {
	i := bytes.Index(buf[start:], pasteEnd)
	if i < 0 {
		// 未终止——注意这一点甚至连 decodeKeyFinal 也不例外，仍然会返回 ok=false，
		// 而这是这条规则唯一被打破的地方。
		//
		// 打破是有意的。转义超时回答的是一个关于人类手指的问题；而粘贴是一台机器
		// 在以管道速度写入，哪怕一兆字节的数据，也要经过很多次读取才能到达，中间
		// 的间隙毫无意义。"超时已过期"根本不能证明粘贴已经结束，所以在这里就此了
		// 结，会把一半的有效载荷当文本发出，再把剩下的部分解码成一个个按键——包括
		// 其中的任何换行符，这在提示符 UI 里意味着提交一个没打完的行。想要安全阀
		// 的调用者，可以自己限制缓冲区大小，丢弃一个长得离谱的未终止粘贴。那是一
		// 种策略，策略该放在调用者那边，就像时钟一样。
		return key{}, 0, false
	}
	n := start + i + len(pasteEnd)
	return key{
		Kind: keyPaste,
		Text: string(buf[start : start+i]),
		Raw:  string(buf[:n]),
	}, n, true
	// 值得了解：协议没有转义，所以一个字面上
	// 包含 "\x1b[201~" 的有效载荷会提前结束粘贴。那个漏洞在协议里，
	// 不在这里——终端通过在发送前从粘贴的文本中过滤 ESC 来弥补，
	// 这也是为什么你不能粘贴一个转义序列进终端
	// 并让它执行。
}

// ---------------------------------------------------------------------------
// 鼠标
// ---------------------------------------------------------------------------

// 修饰符和运动位打包到 SGR 按钮字段中。
const (
	mouseShift  = 0x04
	mouseAlt    = 0x08
	mouseCtrl   = 0x10
	mouseMotion = 0x20 // 在拖拽/移动报告时设置
)

// decodeMouse 解析一个 SGR（1006）鼠标报告：按下时 \x1b[<b;x;yM，
// 释放时 \x1b[<b;x;ym。仅 SGR——见 decodeCSI 中的 X10 说明。
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
		// 去掉修饰符和运动位，使 Button 只是一个按钮号，别的什么都不是。跳过这一
		// 步，Ctrl-点击会报告按钮 16，按住左键拖拽会报告按钮 32，而每个针对
		// Button 的 switch 语句都会多出一个 `default:` 分支，悄悄吃掉真正的点击。
		// 滚轮位（0x40）**不**被去掉，因为在这条线上滚轮向上确实是按钮 64——编码
		// 本身就是这么命名的。
		Button: b &^ (mouseShift | mouseAlt | mouseCtrl | mouseMotion),
		X:      x,
		Y:      y,
		// 运动报告到达时最后一个字节是 'M'，所以拖拽在新的单元格上显示为一次按下。
		// 这是故意的：mouseEvent 没有 Drag 字段，调用者把拖拽重建成"一连串按下、
		// 中间不夹释放"，而这正是它本来就必须跟踪的东西，为的是知道哪个按钮正按
		// 着。运动事件只有在调用者请求了模式 1002/1003 时才会到达。
		Press: press,
	}
	return k, n, true
}

// ---------------------------------------------------------------------------
// 参数
// ---------------------------------------------------------------------------

// csiParams 把 CSI 参数字节分割成整数，或报告它们
// 根本不是数字。一个省略的参数变成 -1 而不是 0，因为
// ECMA-48 说一个省略的参数意味着"使用默认值"，而默认值
// 不总是零——\x1b[;5A 是 Ctrl-Up，不是 Ctrl-某-零。
func csiParams(p []byte) ([]int, bool) {
	if len(p) == 0 {
		return nil, true
	}
	out := make([]int, 0, 4)
	for _, f := range bytes.Split(p, []byte{';'}) {
		// xterm 的 modifyOtherKeys 和 kitty 的协议在冒号后
		// 打包子参数（\x1b[1;5:3A 区分按下和重复）。这里
		// 没有任何东西需要它们，而丢弃它们比拒绝序列更好：
		// 基础键和它的修饰符仍然非常可读，
		// 一个选择加入更丰富协议的终端不应该变成
		// 一个箭头键停止工作的终端。
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

// applyModifier 解码 xterm 修饰符参数。
//
// 编码是一个位掩码加一，而那个加一是终端代码中
// 最可靠的差一错误。未修饰是 1，不是 0，
// 所以你必须在掩码前减去。忘了它，
// 每个修饰符都被读为下一个：Ctrl-Up（5）带着 shift 来，
// Shift-Right（2）作为 Alt 来，结果的 bug 令人发疯，
// 恰恰因为箭头仍然"工作"——它们只是工作时用了
// 错误的修饰符，在某个终端，有时。
func applyModifier(k *key, mod int) {
	if mod <= 1 {
		return
	}
	m := mod - 1
	k.Shift = m&1 != 0
	k.Alt = m&2 != 0
	k.Ctrl = m&4 != 0
	// 第 8 位是 Meta。忽略：这十年的终端中没有任何东西发出它，
	// 而把一个虚拟修饰符折叠进 Alt 会使 Alt 变得不可靠。
}

// ---------------------------------------------------------------------------
// 调试渲染。存在的目的是让一个失败的测试说"想要 keyUp，得到
// keyUnknown raw=\"\\x1bOA\""，而不是"想要 6，得到 20"——
// 这是修复和整个下午的区别。
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
