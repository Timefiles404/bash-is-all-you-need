// 阶段 12——不再跑第二遍。
//
// 先提个醒：这个仓库里现在有两样毫不相干的东西都叫缓存，认岔了要搭进去一整
// 个下午。
//
//	阶段 04 的缓存  是**供应商**的 prompt 缓存。它在对方那边，它计费，
//	                render.go 里的 hitRate() 量的是它。
//	阶段 12 的缓存  是我们自己的。它在这个进程里，存的是某条命令产出的
//	                文本，而它的任何东西都不上线。
//
// 想法一句话说得完：模型要跑的命令我们跑过了，而它读过的东西一样没变，那就
// 把上次的答案递回去，别再跑一遍。
//
// 难处全在后半句。"跑过了"得先定义两条命令什么时候算同一条。"读过的东西没
// 变"得先知道它读了什么——工具是 bash，一般情况下你根本无从知道。阶段 08 从
// 安全那一侧撞见的是同一件事：光读一条 shell 命令，判不出它会干什么。区别在
// 于两边允许朝哪个方向坏。黑名单坏起来是放行，跑掉的是危险的东西；白名单坏
// 起来是拦下，跑掉的是那条命令本身——反正它本来也要跑。所以这个文件整个是拿
// 拒绝搭起来的，而这么搭的代价，docs/12-echo.md 量了。
package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 裁决
// ---------------------------------------------------------------------------

// cacheVerdict 是查一条命令时发生了什么。
//
// 四个值，一种事件类型——上下文压缩和分诊各自占了三种，这里不是。判据是这些
// 值答不答同一个问题。这四个答的是同一个："这条命令跑了没有，没跑是为什
// 么"。先例是 KindGateVerdict，它也是这样带着 allow/deny/abort 的。
//
// refused 和 miss 分开是故意的，而这一分，正是这个类型没写成 bool 的全部理
// 由。未命中说的是缓存本来帮得上，只是还没拿到答案；连着十次未命中，那是冷
// 缓存在预热。拒绝说的是这条命令再跑多少遍缓存也帮不上；而一整场会话全是拒
// 绝，意思是资格规则对眼下这些活儿定得太窄——这是另一个完全不同的问题，可只
// 要你把两者一并记成"没命中"，它就看不见了。
type cacheVerdict string

const (
	cacheHit     cacheVerdict = "hit"
	cacheMiss    cacheVerdict = "miss"
	cacheStale   cacheVerdict = "stale"   // 有，但它读过的东西变了
	cacheRefused cacheVerdict = "refused" // 没资格，而且永远不会有
)

// ---------------------------------------------------------------------------
// 见证
// ---------------------------------------------------------------------------

// 见证是一条路径：缓存下来的答案依赖它的内容。见证一并记下命令跑的那一刻，
// 这条路径的摘要是什么。
//
// 文件的摘要是内容哈希；目录的摘要是它下一层列表的哈希——名字、大小、权限、
// mtime，大致就是 `ls -l` 打出来的东西，因而也大致就是一次 `ls` 的结果所依
// 赖的东西。
//
// 它**不是** (size, mtime)，而这是量出来的，不是偏好。在这台机器上，先后写
// 进的 "route2:x" 和 "route3:y"——长度一样，字节不同——在 2000 次试验里有
// 1498 次，透过 (size, mtime) 看是一模一样的：这个文件系统交回来的 mtime 以
// 半毫秒左右为一档跳动，而一次改写落在一档之内。内容哈希的开销是 stat 的 2
// 到 4 倍（149 B 到 50 KB 的文件上，17µs 对 34–68µs）；对一条中位数 92 ms
// 的命令来说，这不过是个舍入误差，而它买到的是正确性。
type witness struct {
	Path   string
	Digest string
}

// digestOf 按读它的人看到的样子给一条路径算摘要。
//
// 路径不存在不算错：它返回 "" 且不返回错误，于是见证一旦消失，跟文件还在时
// 记下的那个摘要一比就不相等。在这里返回错误，等于把"文件被删了"说成缓存出
// 了故障，而不是世界上的一桩事实；调用方随后就得拿同一套办法去对付两件不同
// 的事。
func digestOf(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if fi.IsDir() {
		ents, err := os.ReadDir(path)
		if err != nil {
			return ""
		}
		h := sha256.New()
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		sort.Strings(names) // ReadDir 本来就排了序；别指望它一直这样
		for _, n := range names {
			sub, err := os.Lstat(filepath.Join(path, n))
			if err != nil {
				fmt.Fprintf(h, "%s\x00?\x00", n)
				continue
			}
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00", n, sub.Size(), sub.Mode(), sub.ModTime().UnixNano())
		}
		return "d:" + hex.EncodeToString(h.Sum(nil))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "f:" + hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

type echoEntry struct {
	key       string
	command   string
	text      string // 第一次告诉模型的那些字节，一个不差
	witnesses []witness
	millis    int64 // 这条命令真跑的那天花了多少
	stored    time.Time
	el        *list.Element
}

// resultCache 是架在命令结果上的 LRU。一个 agent 和它全部的子 agent 共用同
// 一份。
//
// 按指针共用，这是故意的，也是共用缓存明摆着划算的那一处：阶段 07 会在同一
// 棵工作树上一次扇出好几个孩子，而三个孩子各自打开同一个文件，就是三条相隔
// 几微秒的、一模一样的命令。每个 agent 一份缓存，这三条一条都接不住。
//
// 上界有两道——条目数和总字节数——因为这两样是在不同时候用光的。一场会话拿
// `wc -l` 扫过四百个文件，四十字节一条的答案就把条目数填满了；一场会话读四
// 个大文件，四个条目就把字节预算填满了。只设一道，另一道就没人管；而长会话
// 里不设上界的结果缓存，是一处出于好意的内存泄漏。
type resultCache struct {
	mu      sync.Mutex
	entries map[string]*echoEntry
	order   *list.List // 队首 = 最近用过的
	bytes   int

	maxEntries int
	maxBytes   int

	// ttl 是兜底，默认关着，而这段注释比这个字段更要紧。
	//
	// 见证集合是命令实际读过的东西的**下界**：它装的是命令行上点了名的
	// 路径，而一条命令可以依赖任何路径都没点到的东西。TTL 限的是错答案
	// 能在这条缝里活多久。
	//
	// 它绝不能当主力，而"为什么"有一份现场报告。某个 agent 把 `git log`
	// 压在 15 秒的 TTL 后面，而它的调用方每 30 秒才重取一次；每个条目被
	// 问到的时候都早已过期，于是命中率恰好是零——一连几个月，没有错答
	// 案，没有报错，任何日志里都没有一个字。除非你去数命中，否则从不命
	// 中的缓存和正常干活的缓存长得一模一样——这就是这个文件数它们、并且
	// 把数出来的结果打出来的原因。
	ttl time.Duration

	stats cacheStats
}

type cacheStats struct {
	Lookups     int
	Hits        int
	Stale       int
	Refused     int
	Expired     int
	Stored      int
	Evicted     int
	BytesServed int
	SavedMillis int64
}

func newResultCache(maxEntries, maxBytes int, ttl time.Duration) *resultCache {
	return &resultCache{
		entries:    map[string]*echoEntry{},
		order:      list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		ttl:        ttl,
	}
}

// keyOf 就是"同一条命令"的定义。
//
// 凡是能改变答案的都算进去，包括四个在这个进程活着期间根本变不了的值：
// shell、工作目录、输出预算和环境变量。把常量放进键里，看着像迷信。它是给下
// 一个特性买的保险：哪天有人把这份缓存落盘，或者给某个子 agent 单开一个工作
// 目录，这四样就不再是常量了，而一把只是碰巧正确的键，会开始拿这个目录的答
// 案去回答另一个目录。
//
// maxOutput 在里面，是因为存下来的文本是**渲染过**的结果，已经截断到装得
// 下。同一条命令换一个 --max-output，产出的字节就不同，而模型读的正是这些字
// 节。
//
// 有意不进键的：时间、回合序号、发问的是哪个 agent。这些要是有影响，这个条
// 目就根本谈不上重用了。
func keyOf(shell, wd, command string, maxOutput int, env []string) string {
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	h := sha256.New()
	fmt.Fprintf(h, "v1\x00%s\x00%s\x00%d\x00", shell, wd, maxOutput)
	for _, e := range sorted {
		fmt.Fprintf(h, "%s\x00", e)
	}
	fmt.Fprintf(h, "\x00%s", command)
	return hex.EncodeToString(h.Sum(nil))
}

// cacheLookup 是一个答案，形状照调用方的两样需要来定：拿它做事，以及在
// trace 里把它说清楚。
//
// key 在这儿，是因为调用方回头存结果时还要用一次；重算一遍，就是白白让每条
// 命令把环境变量哈希两遍。
//
// millis 是这条命令真跑那天花掉的时间，也是"一次命中省下了多少"唯一老实的
// 说法。另一种做法——给查找计时，再把差值叫作节省——什么也没量到，因为那条
// 没跑的命令根本没有时长可以拿来相减。
//
// before 装的是命令跑**之前**取的见证摘要，这半边是靠一个失败的测试才找出
// 来的。见 store。
type cacheLookup struct {
	key     string
	text    string
	verdict cacheVerdict
	reason  string
	millis  int64
	before  []witness
}

// lookup 一口气回答三个问题：这条命令到底准不准缓存，我们手上有没有，手上
// 这份还算不算数。
func (rc *resultCache) lookup(shell, wd, command string, maxOutput int, env []string) cacheLookup {
	if rc == nil {
		return cacheLookup{verdict: cacheRefused, reason: "cache disabled"}
	}
	paths, ok, why := eligible(command, wd)
	if !ok {
		rc.mu.Lock()
		rc.stats.Lookups++
		rc.stats.Refused++
		rc.mu.Unlock()
		return cacheLookup{verdict: cacheRefused, reason: why}
	}

	key := keyOf(shell, wd, command, maxOutput, env)

	rc.mu.Lock()
	rc.stats.Lookups++
	e, have := rc.entries[key]
	if !have {
		rc.mu.Unlock()
		return cacheLookup{key: key, verdict: cacheMiss, before: digestAll(paths)}
	}
	if rc.ttl > 0 && time.Since(e.stored) > rc.ttl {
		rc.dropLocked(e)
		rc.stats.Expired++
		rc.mu.Unlock()
		return cacheLookup{key: key, verdict: cacheMiss, reason: "expired", before: digestAll(paths)}
	}
	stored := e.witnesses
	rc.mu.Unlock()

	// 哈希在锁外面做。它要碰磁盘，而在一份几个子 agent 共用的缓存里，把
	// 互斥锁一直攥到文件系统调用做完，缓存就会比它替掉的那个东西还慢。
	//
	// 这样让出来的竞态是真的，也是良性的：我们算哈希的时候，另一个
	// goroutine 可能把这个条目淘汰掉。下面那次重查就是让它安全的东西，
	// 而它坏起来最多白查一次，不会给出错答案。
	changed := ""
	for _, w := range stored {
		if d := digestOf(w.Path); d != w.Digest {
			changed = w.Path
			break
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, have = rc.entries[key]
	if !have {
		return cacheLookup{key: key, verdict: cacheMiss, before: digestAll(paths)}
	}
	if changed != "" {
		rc.dropLocked(e)
		rc.stats.Stale++
		return cacheLookup{key: key, verdict: cacheStale, reason: changed, before: digestAll(paths)}
	}
	rc.order.MoveToFront(e.el)
	rc.stats.Hits++
	rc.stats.BytesServed += len(e.text)
	rc.stats.SavedMillis += e.millis
	return cacheLookup{key: key, text: e.text, verdict: cacheHit, millis: e.millis}
}

func witnessPaths(ws []witness) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Path)
	}
	return out
}

func digestAll(paths []string) []witness {
	ws := make([]witness, 0, len(paths))
	for _, p := range paths {
		ws = append(ws, witness{Path: p, Digest: digestOf(p)})
	}
	return ws
}

// store 存下一个结果，而它拒绝的次数远多于存下的次数。
//
// 有意思的是拒绝的那一半：
//
//   - 退出码非零不存。退出码讲的是一次遭遇，不是答案；而会重复出现的那些
//     遭遇，恰恰是你最不想冻起来的：一次权限抽风，一个文件边被写边被读，
//     一块磁盘满了一分钟。反过来选的现场报告是有的——某个索引把解析
//     **失败**按内容哈希缓存了起来，于是修好解析器之后一个文件也没重新
//     索引，因为每个文件哈希出来还是当初失败的那串字节。那是拿正确性换
//     停机，而且是有意换的；这里是同一笔交易的另一面。
//   - 超时、取消，或者进程没被回收，同样不存，理由一样，而且更硬。这几种文
//     本没有一种是在讲这条命令干什么；它们讲的是它那一次遭遇了什么。
//   - 键是空的，意味着 lookup 拒绝了这条命令，那就没有东西可以拿来挂它。
//   - 命令读着读着被改掉的文件不存——这一条是撞出来的，不是想出来的。见下。
//
// 见证要哈希两遍：一遍在 lookup 里，命令跑之前；一遍在这儿，命令跑完之后。
// 两遍都少不了，单拿哪一遍都不够。
//
// 只取读**之后**的摘要：文件读到一半被改，给出的结果是撕裂的，而这份结果随
// 后被挂在文件最终那个摘要底下——于是下一次查找找到一个对得上的见证，端出一
// 份结果，而这份结果从未对应过该文件的任何一个状态。缓存这下错得理直气壮，
// 而且一直错到那个文件再次被改。
//
// 只取读**之前**的摘要：错的东西是端不出去了，可文件要是在查找和命令之间被
// 改过，它就被挂在自己已经不再拥有的摘要底下，于是条目一生下来就是失效的，
// 此后每一次查找都把命令重跑一遍。安全，而且一声不响地毫无用处。
//
// 把两遍比一比，代价是多做一次哈希——68µs，对一条中位数 92 ms 的命令——而两
// 边对不上就不存；只有这个版本既不出错，也不白干。
func (rc *resultCache) store(look cacheLookup, command, text string, r execResult) {
	if rc == nil || look.key == "" {
		return
	}
	// 命中不带 `before` 摘要，因为命中时什么都没跑，也就没有东西可比。
	// 照它去存，写下的条目见证集合是**空的**——这种条目永远不会失效，因
	// 为它什么也没盯着。runCommand 到不了这儿就返回了，所以今天这道防线
	// 走不到；它摆在这儿，是因为下一个调用方不会知道这件事，而它挡下的
	// 那种故障，不吭声，也不会自己好。
	if look.verdict == cacheHit {
		return
	}
	if r.ExitCode != 0 || r.TimedOut || r.Cancelled || r.Unreaped {
		return
	}
	ws := digestAll(witnessPaths(look.before))
	if len(ws) != len(look.before) {
		return
	}
	for i := range ws {
		if ws[i] != look.before[i] {
			return // 它在命令底下变了；这段文本什么也没描述
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if old, ok := rc.entries[look.key]; ok {
		rc.dropLocked(old)
	}
	key := look.key
	e := &echoEntry{
		key: key, command: command, text: text, witnesses: ws,
		millis: r.Duration.Milliseconds(), stored: time.Now(),
	}
	e.el = rc.order.PushFront(e)
	rc.entries[key] = e
	rc.bytes += len(text)
	rc.stats.Stored++

	for (rc.maxEntries > 0 && rc.order.Len() > rc.maxEntries) ||
		(rc.maxBytes > 0 && rc.bytes > rc.maxBytes) {
		back := rc.order.Back()
		if back == nil {
			break
		}
		rc.dropLocked(back.Value.(*echoEntry))
		rc.stats.Evicted++
	}
}

func (rc *resultCache) dropLocked(e *echoEntry) {
	rc.order.Remove(e.el)
	delete(rc.entries, e.key)
	rc.bytes -= len(e.text)
}

func (rc *resultCache) snapshot() cacheStats {
	if rc == nil {
		return cacheStats{}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.stats
}

// ---------------------------------------------------------------------------
// 拿已经发生过的会话来审计一份缓存
// ---------------------------------------------------------------------------

// cacheAudit 回答的是开缓存之前唯一值得问的那个问题：它当初本来能帮上忙吗？
//
// 它不要 API key，也不要模型。这个仓库能产出的每一份 trace，都按顺序记下了
// 每一次工具调用的确切命令和它的开销——而缓存这个决定所依赖的，全在这里面
// 了。把这些命令放进一份冷缓存里重放一遍，得到的就是你当初本来会有的命中
// 率，量的是你自己的活儿，不是别人的基准测试。
//
// trace 唯一没留的是每条命令打出来的文本，只留了长度，所以这里存进去的正文
// 是凑到那个长度的填充物。这对字节账毫无影响，对命中账也毫无影响：键是从命
// 令算出来的，从来不看结果。
//
// 它模拟不了的，是会话中途有文件被改——trace 记的是 agent 跑了什么，不记磁
// 盘在底下做了什么。所以它报出来的数字是命中数的上界，这一章也照实这么写。
func cacheAudit(paths []string, wd string, out io.Writer) error {
	fmt.Fprintf(out, "%-24s %6s %6s %6s %6s %6s %10s\n",
		"trace", "cmds", "hit", "miss", "stale", "refus", "saved")
	var tot cacheStats
	var totCmds int
	// 拒绝都是为什么发生的，跨所有 trace 汇总。这是报告里你真正拿来动手
	// 的那一半：命中率是一句裁决，而拒绝的理由是一张待办清单——它点名了
	// 该往哪条规则上加东西，也让你看清加了到底值不值。
	reasons := map[string]int{}
	for _, p := range paths {
		events, err := ReadTrace(p)
		if err != nil {
			return err
		}
		rc := newResultCache(1<<20, 1<<30, 0)
		n := 0
		for _, e := range events {
			if e.Kind != KindCommandEnd {
				continue
			}
			n++
			look := rc.lookup("audit", wd, e.Command, 8000, nil)
			if look.verdict == cacheHit {
				continue
			}
			if look.verdict == cacheRefused {
				reasons[look.reason]++
			}
			rc.store(look, e.Command, strings.Repeat("x", e.Bytes),
				execResult{ExitCode: e.ExitCode, TimedOut: e.TimedOut,
					Duration: time.Duration(e.Millis) * time.Millisecond})
		}
		s := rc.snapshot()
		fmt.Fprintf(out, "%-24s %6d %6d %6d %6d %6d %10v\n", filepath.Base(p), n,
			s.Hits, s.Lookups-s.Hits-s.Stale-s.Refused, s.Stale, s.Refused,
			time.Duration(s.SavedMillis)*time.Millisecond)
		totCmds += n
		tot.Lookups += s.Lookups
		tot.Hits += s.Hits
		tot.Stale += s.Stale
		tot.Refused += s.Refused
		tot.BytesServed += s.BytesServed
		tot.SavedMillis += s.SavedMillis
	}
	fmt.Fprintf(out, "%-24s %6d %6d %6d %6d %6d %10v\n", "TOTAL", totCmds,
		tot.Hits, tot.Lookups-tot.Hits-tot.Stale-tot.Refused, tot.Stale, tot.Refused,
		time.Duration(tot.SavedMillis)*time.Millisecond)
	if tot.Lookups > 0 {
		fmt.Fprintf(out, "\nhit rate %.1f%% of %d commands · %s of output not re-read · %v of command time not re-run\n",
			float64(tot.Hits)*100/float64(tot.Lookups), totCmds,
			humanBytes(tot.BytesServed), time.Duration(tot.SavedMillis)*time.Millisecond)
	}
	if len(reasons) > 0 {
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if reasons[keys[i]] != reasons[keys[j]] {
				return reasons[keys[i]] > reasons[keys[j]]
			}
			return keys[i] < keys[j]
		})
		fmt.Fprintf(out, "\nrefused, by reason:\n")
		for _, k := range keys {
			fmt.Fprintf(out, "  %3d  %s\n", reasons[k], k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 资格
// ---------------------------------------------------------------------------

// eligible 决定一条命令准不准缓存；准的话，它的答案又依赖哪些路径。
//
// 它是一个解析器，凡是没完全看懂的一律拒绝。这跟阶段 08 那个正则的形状正好
// 相反：那个是去认出危险的写法，其余的放行；那种形状的规则，对现存的每一条
// 命令都必须判对，而这一个只需要对它收下的那些判对。
//
// 后果是故意摆在明面上的。`sed -n '/word/p'` 会被拒，因为规则分不清 "word"
// 里的 `w` 和 sed 那个往文件里写的 `w` 命令。一次误拒的代价是一条命令——
// 92 ms，是在十六场真实会话上量出来的中位数。一次误收的代价是往用户磁盘上
// 写了东西，再把这次写入从缓存里端出来。这条规则只许朝一个方向犯傻。
func eligible(command, wd string) (paths []string, ok bool, why string) {
	stages, err := splitPipeline(command)
	if err != nil {
		return nil, false, err.Error()
	}
	if len(stages) == 0 {
		return nil, false, "empty command"
	}
	seen := map[string]bool{}
	for i, argv := range stages {
		rule, known := readers[argv[0]]
		if !known {
			return nil, false, "not a known read-only program: " + argv[0]
		}
		args, err := rule.check(argv[1:])
		if err != nil {
			return nil, false, argv[0] + ": " + err.Error()
		}
		if i == 0 && len(args) == 0 && !rule.cwdIsInput {
			// 管道头一节没有文件参数，读的就是标准输入，而 runBash 把
			// 标准输入设成了空。这确实是确定的，可它确定只是因为另一
			// 个文件里做过一个决定；缓存依赖这种细节，离出错只差一次
			// 重构。
			return nil, false, argv[0] + ": no path named"
		}
		if len(args) == 0 && rule.cwdIsInput {
			args = []string{"."}
		}
		for _, a := range args {
			p := a
			if !filepath.IsAbs(p) {
				p = filepath.Join(wd, p)
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths, true, ""
}

// readerRule 是一个程序，外加允许对它说的全部话。
//
// 白名单的单位不是程序。`sed` 是读，`sed -i` 是写；`sort` 是读，`sort -o`
// 是写。要是列一张程序名的清单，上面六个程序里有两个会被判错。
type readerRule struct {
	// 不带值的标志，以及会吞掉下一个参数的标志。
	boolFlags  map[string]bool
	valueFlags map[string]bool

	// cwdIsInput 标记这样一种程序：不给参数时它的输入是工作目录，不是
	// 标准输入。这里只有 `ls` 一个。
	cwdIsInput bool

	// scriptArgs 是开头有几个非标志参数其实是程序、不是路径——sed 和
	// grep 是 1，其余都是 0。这个弄错了，一段 sed 脚本就会被当成文件塞
	// 进见证集合，而这个见证会永远哈希成 ""，读起来就是永久失效：一份
	// 从不命中、也从不说明为什么的缓存。
	scriptArgs int

	// scriptSafe 筛这些参数。为 nil 表示不需要筛。
	scriptSafe func(string) error
}

var readers = map[string]readerRule{
	"cat": {boolFlags: set("-n", "-b", "-s", "-A", "-e", "-t", "-v", "-E", "-T")},
	"head": {boolFlags: set("-q", "-v", "-z"),
		valueFlags: set("-n", "-c")},
	"tail": {boolFlags: set("-q", "-v", "-z"),
		valueFlags: set("-n", "-c")},
	"wc":  {boolFlags: set("-l", "-w", "-c", "-m", "-L")},
	"nl":  {boolFlags: set("-p"), valueFlags: set("-b", "-w", "-s", "-v", "-i")},
	"cut": {boolFlags: set("-s", "-n"), valueFlags: set("-d", "-f", "-b", "-c", "--output-delimiter")},
	"ls":  {boolFlags: set("-l", "-a", "-A", "-h", "-t", "-r", "-S", "-1", "-la", "-al", "-lh", "-lha", "-alh", "-ltr", "-F", "-d"), cwdIsInput: true},
	// sort -o 会写文件，所以 -o 不在表上；而认不出来的标志走的是拒绝，
	// 不是耸耸肩放过去。
	"sort": {boolFlags: set("-n", "-r", "-u", "-h", "-b", "-f", "-V", "-g"),
		valueFlags: set("-k", "-t", "-S")},
	"uniq": {boolFlags: set("-c", "-d", "-u", "-i"), valueFlags: set("-f", "-s", "-w")},

	"sed": {
		boolFlags:  set("-n", "-E", "-r"),
		valueFlags: set("-e"),
		scriptArgs: 1,
		scriptSafe: sedScriptSafe,
	},
	"grep": {
		// -r 和 -R 是故意没有的。它们的见证集合是一整棵树，那不是一
		// 张路径列表装得下的东西；而一份悄悄不完整的见证集合，比没有
		// 缓存还糟：它端出来的是理直气壮的失效答案，换掉了慢而正确的
		// 答案。
		boolFlags:  set("-n", "-i", "-c", "-v", "-E", "-F", "-l", "-L", "-h", "-H", "-o", "-w", "-x", "-s", "-a", "-q"),
		valueFlags: set("-m", "-A", "-B", "-C", "-e"),
		scriptArgs: 1,
		scriptSafe: nil,
	},
}

// check 校验标志，并把其中属于路径的参数返回。
func (r readerRule) check(args []string) ([]string, error) {
	var paths []string
	scriptsLeft := r.scriptArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, rest := range args[i+1:] {
				paths = append(paths, rest)
			}
			return paths, nil
		case strings.HasPrefix(a, "-") && a != "-":
			name := a
			if eq := strings.IndexByte(a, '='); eq > 0 {
				name = a[:eq]
			}
			if r.valueFlags[name] {
				if name == a { // 值就是下一个参数
					if i+1 >= len(args) {
						return nil, fmt.Errorf("flag %s has no value", a)
					}
					if name == "-e" && r.scriptSafe != nil {
						if err := r.scriptSafe(args[i+1]); err != nil {
							return nil, err
						}
						scriptsLeft = 0
					}
					i++
				}
				continue
			}
			if r.boolFlags[name] && name == a {
				continue
			}
			// 数字简写：head -20、tail -5。它不是路径，也不是这条规则
			// 非得一个个认识的标志。
			if isNumericFlag(a) && (r.valueFlags["-n"] || r.valueFlags["-c"]) {
				continue
			}
			// 挤在一起的短标志：`grep -oE` 就是 -o 和 -E，而模型天天这么
			// 写。只有当每个字母都是这条规则本来就允许的布尔标志才收下，
			// 所以 `grep -oP` 仍然因为那个 -P 被拒，一串里结尾是个要带值
			// 的标志的，也是拒掉而不是去猜。
			//
			// 这个分支是被一份拒绝清单要来的。给这一章跑的那几场会话，审计
			// 报出三条拒绝：`unknown flag -oE`、`-oP`、`-noiE`。那不是三次
			// 意外，那是一整类漏网——而这正是一条值得动手的理由和一条凑合
			// 活着的理由之间的差别。
			if bundleOK(a, r) {
				continue
			}
			return nil, fmt.Errorf("unknown flag %s", a)
		case scriptsLeft > 0:
			scriptsLeft--
			if r.scriptSafe != nil {
				if err := r.scriptSafe(a); err != nil {
					return nil, err
				}
			}
		default:
			paths = append(paths, a)
		}
	}
	return paths, nil
}

// bundleOK 回答的是：`-abc` 是不是三个这条规则允许的布尔标志。
func bundleOK(a string, r readerRule) bool {
	if len(a) < 3 || a[0] != '-' || a[1] == '-' {
		return false
	}
	for _, c := range a[1:] {
		if c > 127 || !r.boolFlags["-"+string(c)] {
			return false
		}
	}
	return true
}

func isNumericFlag(a string) bool {
	if len(a) < 2 || a[0] != '-' {
		return false
	}
	for _, c := range a[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// sedScriptSafe 拒绝这样的脚本：里面有某个字母，可能是一条碰文件或者碰进程
// 的 sed 命令。
//
//	w  把模式空间写进文件      W  写第一行
//	r  读入一个文件            R  读一个文件的一行
//	e  执行一条 shell 命令（GNU 扩展）
//
// 它看的是每一个字符，不看命令位置，所以 `/word/p` 和 `s/read/x/` 跟
// `1,5w out.txt` 一起被拒。这是不写 sed 解析器的代价——写了才判得出缓存该不
// 该跳过一条 92 ms 的命令——而这笔账的方向是对的：拒绝用毫秒计，另一边用用
// 户的文件计。
func sedScriptSafe(script string) error {
	if i := strings.IndexAny(script, "wWrRe"); i >= 0 {
		return fmt.Errorf("script contains %q, which could be a file or exec command", script[i])
	}
	return nil
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// 分词器
// ---------------------------------------------------------------------------

// splitPipeline 把一条命令切成管道各节的参数表；切不动就返回错误，点名第一
// 个它没看懂的东西。
//
// 它认识单引号、双引号、反斜杠转义和管道符，别的一概不认。其余每一种 shell
// 构造都是错误——也就是说，凡是能重定向输出、能再跑一条命令、能把某条命令的
// 输出替换进来、能展开变量、能开一个子 shell 的构造，全是错误。这张清单短，
// 只是因为收下的文法小。
//
// 这不是一个 shell，也不许长成一个。哪天它非得看懂 `$(...)` 才有用了，老实
// 的修法是搬阶段 08 那个真解析器，不是往这个 switch 里再加一个 case。
func splitPipeline(command string) ([][]string, error) {
	var stages [][]string
	var argv []string
	var cur strings.Builder
	quoted := false // 这个参数已经开头了，哪怕它是空的

	flush := func() {
		if cur.Len() > 0 || quoted {
			argv = append(argv, cur.String())
			cur.Reset()
			quoted = false
		}
	}
	endStage := func() error {
		flush()
		if len(argv) == 0 {
			return fmt.Errorf("empty pipeline stage")
		}
		stages = append(stages, argv)
		argv = nil
		return nil
	}

	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case '\'':
			quoted = true
			j := strings.IndexByte(command[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(command[i+1 : i+1+j])
			i += j + 1
		case '"':
			quoted = true
			// 双引号字符串里只有两个字符会让它从字面量变成程序；一个
			// 都不含的时候，它才是安全的。
			//
			// 反斜杠的规则跟引号外面那套不一样，而弄错它，正是这个函
			// 数一声不响造出一堆不存在的路径的原因。双引号里面，bash
			// 只在 $ ` " \ 和换行符前面把反斜杠当转义符；在别的东西前
			// 面它就是个普通字符。所以 `cat "D:\Projects\x.md"` 是一
			// 条 Windows 路径；分词器要是见反斜杠就删，就会把它变成
			// `D:Projectsx.md`——一条永远哈希成空的路径，真文件出什么
			// 事都一样。什么都不会报错。见证只是什么也没盯着而已。
			for i++; i < len(command) && command[i] != '"'; i++ {
				switch command[i] {
				case '$', '`':
					return nil, fmt.Errorf("substitution inside double quotes")
				case '\\':
					if i+1 >= len(command) {
						return nil, fmt.Errorf("trailing backslash")
					}
					if next := command[i+1]; next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
						i++
					}
					cur.WriteByte(command[i])
					continue
				}
				cur.WriteByte(command[i])
			}
			if i >= len(command) {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case '\\':
			if i+1 >= len(command) {
				return nil, fmt.Errorf("trailing backslash")
			}
			i++
			quoted = true
			cur.WriteByte(command[i])
		case ' ', '\t':
			flush()
		case '|':
			if i+1 < len(command) && command[i+1] == '|' {
				return nil, fmt.Errorf("|| is a control operator")
			}
			if err := endStage(); err != nil {
				return nil, err
			}
		case '$', '`', ';', '&', '<', '>', '(', ')', '{', '}', '\n', '\r', '#', '*', '?', '[', ']', '~', '!':
			// 通配符跟控制操作符一起列在这张表里，理由是相通的：
			// `cat *.md` 点的是一组文件，而解析它们的是 shell，于是这
			// 个函数返回的路径不会是命令实际读的路径；目录里新出现一
			// 个文件，会改变答案，却不改变任何一个见证。
			return nil, fmt.Errorf("unsupported shell character %q", string(c))
		default:
			cur.WriteByte(c)
		}
	}
	if err := endStage(); err != nil {
		return nil, err
	}
	return stages, nil
}
