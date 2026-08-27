// 阶段 07 — 技能：一个目录，外加一段说明。
//
// "技能"听起来像一个子系统：一个注册表、一个加载器、一个匹配器，
// 说不定还有一个嵌入模型。它一个都不是。一个技能就是：
//
//	一个 Markdown 文件，加一句告诉模型它存在的话。
//
// 模型在判断这个技能适用时，会用 `cat` 去读正文。这里没有技能工具，
// 没有检索步骤，也没有运行时——这和阶段 05 关于记忆所做的是同一个
// 观察，只是这次从另一个方向抵达。一旦 Agent 有了 shell，"在相关时
// 加载这份文档"就不是一个需要你去构建的功能，它就是一个文件名。
//
// 真正承重的，是这个**形状**：名称和描述始终在上下文里，正文只在
// 需要时才读。这就是**渐进披露**，也正是一个项目能够拥有 40 个
// 技能、却不需要一份 40 技能份量的 prompt 的唯一原因。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type skill struct {
	Name        string
	Description string
	Path        string // 相对的，因为模型必须能用 `cat` 读到它。
	BodyBytes   int    // 把它加载进来要花多少成本，供计费之用。
}

// loadSkills 扫描的是 skills/<name>/SKILL.md。
//
// 每个技能一个目录，而不是每个技能一个平面文件，因为一个真正的技能
// 会长出附件——一个它会让模型去运行的脚本，一个模板，一个示例输入。
// 那些文件就活在它旁边，模型可以用 `ls` 找到它们，因为它已经知道
// 这个目录。
func loadSkills(root string) []skill {
	dir := filepath.Join(root, "skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name, desc := parseFrontmatter(string(raw))
		if name == "" {
			name = e.Name()
		}
		if desc == "" {
			// 一个没有描述的技能是隐形的：索引是模型唯一能看到的东西，所以缺了
			// 描述就意味着这个技能永远不会被选中。悄悄跳过它，会把这一点藏起来；
			// 要是在索引里用一句明确的抱怨去点名它，就等于永远地把这句抱怨放进
			// 每一个请求里。选择跳过，情愿让 skills_indexed 事件里的计数，和
			// 实际的目录列表对不上。
			continue
		}
		out = append(out, skill{
			Name:        name,
			Description: desc,
			Path:        filepath.ToSlash(filepath.Join("skills", e.Name(), "SKILL.md")),
			BodyBytes:   len(raw),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseFrontmatter 从开头的 `---` 块中读取 `name:` 和 `description:`。
//
// 二十行代码代替一个 YAML 依赖。这笔交易值得讲明，因为这正是整个仓库在做
// 的交易。YAML 可以处理嵌套值、锚点、多行标量和类型强制转换——但两个字符
// 串字段都用不上。代价是在一个"你可以读懂所有代码"的项目中引入依赖，来解
// 析一个我们也能控制的文件格式。当你掌握一个接口的两端时，解析器就可以做
// 得和这个接口一样小。
//
// 失败模式很诚实：任何不理解的东西都被忽略，没有描述的技能不会出现。
func parseFrontmatter(s string) (name, description string) {
	// Windows 上编写的技能文件经常以 UTF-8 BOM 开头，字面的 U+FEFF 在 Go 源
	// 文件中除了字节零之外的任何位置都是编译错误，所以截集用 rune 值拼写：
	// BOM、空格、制表符、CR、LF。
	s = strings.TrimLeft(s, string([]rune{0xFEFF, 0x20, 0x09, 0x0D, 0x0A}))
	if !strings.HasPrefix(s, "---") {
		return "", ""
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	return name, description
}

// skillsPrompt 渲染进入系统提示词的索引。
//
// 三条指令，每一条都源于出错的某种方式：
//
//   - "在行动前读完正文" —— 否则模型会作用在描述上，那只有一行，是为了可
//     选择而写的，不是为了内容详尽。
//   - "最多一个" —— 给模型五个合理的技能，它会读所有五个，这把节省 token
//     变成了花费 token 加五次往返。
//   - "如果都不适用，忽略它们" —— 没有这条，技能列表读起来像个模型应该下
//     单的菜单，它会找到一个勉强符合的。
func skillsPrompt(skills []skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n<skills>\nThis project has playbooks for recurring tasks. Only their names and one-line\ndescriptions are below. If one clearly applies to what you are doing, read it\nfirst with `cat`, then follow it.\n\n")
	w := 0
	for _, s := range skills {
		if len(s.Path) > w {
			w = len(s.Path)
		}
	}
	for _, s := range skills {
		fmt.Fprintf(&b, "  %-*s  %s\n", w, s.Path, s.Description)
	}
	b.WriteString("\nRead at most one before acting. If none clearly applies, ignore this list\nentirely — it is a set of shortcuts, not a menu you have to order from.\n</skills>")
	return b.String()
}

// skillsCost 报告索引的成本和加载所有内容的成本。
//
// 值得打印，因为索引**不是免费**的，算术本身就是整个设计决策。每个技
// 能的名称和描述，在整个会话期间，都写在每一次请求的前缀里。四十个技
// 能是几千 token 的永久开销——缓存后价格是阶段 04 后的十分之一，但从不
// 为零。一个没人修剪、只会不断膨胀的技能目录，就是向 Agent 的每一次调
// 用征收的税，唯一能让人注意到这一点的办法，是有什么东西把这个数字打
// 印出来。
func skillsCost(skills []skill) (indexBytes, bodyBytes int) {
	indexBytes = len(skillsPrompt(skills))
	for _, s := range skills {
		bodyBytes += s.BodyBytes
	}
	return indexBytes, bodyBytes
}
