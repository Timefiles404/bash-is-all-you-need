// 阶段 07——技能，就是一个目录加一段话。
//
// "技能"听起来像个子系统：注册表、加载器、匹配器，说不定还有嵌入模
// 型。一个都不是。技能就是：
//
//	一份 Markdown 文件，外加一句话告诉模型它存在。
//
// 模型自己判断这个技能用得上，就用 `cat` 去读正文。没有技能工具，没
// 有检索环节，也没有运行时——这和阶段 05 关于记忆的那个观察是同一
// 条，只是从另一头走过来的。Agent 一旦有了 shell，"相关的时候把这份
// 文档加载进来"就不是你要去做的功能了。它是个文件名。
//
// 真正承重的是那个**形状**：名称和描述常驻上下文，正文按需才读。这
// 就是**渐进披露**，也是项目能有四十个技能、却不必写四十个技能的
// prompt 的唯一原因。
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
	Path        string // 相对路径，因为模型得能 cat 它
	BodyBytes   int    // 加载它要花多少，用来记账
}

// loadSkills 扫的是 skills/<name>/SKILL.md。
//
// 每个技能一个目录，而不是一个技能一个平铺的文件，因为真用起来的技
// 能会长出附件——它让模型去跑的脚本、模板、示例输入。那些就放在它旁
// 边，模型用 `ls` 就能找到，因为它已经知道目录了。
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
			// 没有 description 的技能是隐形的：模型能看到的只有索引，
			// 所以缺了描述就等于这个技能永远不会被选中。默默跳过会把
			// 这件事藏起来；在索引里点名再附一句抱怨，那句抱怨就会永
			// 远待在每次请求里。跳过，然后让 skills_indexed 事件里的
			// 计数对不上目录列表。
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

// parseFrontmatter 从开头的 `---` 块里读出 `name:` 和 `description:`。
//
// 二十行，换掉一份 YAML 依赖，这笔交易值得说清楚，因为整个仓库做的
// 就是同一笔交易。YAML 会处理嵌套值、锚点、多行标量和类型强转——两
// 个字符串字段一样都用不上。而它的代价是：这个项目的全部论据就是
// "你能把它整个读完"，却要为解析一份我们自己也说了算的文件格式添一
// 份依赖。接口两头都归你，解析器就可以小到和接口一样。
//
// 失败方式是诚实的：它不懂的东西一律忽略，没有描述的技能就不出现。
func parseFrontmatter(s string) (name, description string) {
	// 在 Windows 上写的技能文件很常带 UTF-8 BOM 开头，而字面的 U+FEFF 只
	// 要不在 Go 源文件的第 0 字节就是编译错误，所以 cutset 是用 rune 值
	// 写的：BOM、空格、tab、CR、LF。
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

// skillsPrompt 渲染进系统提示词的那份索引。
//
// 三条指令，每一条的存在都对应一种出错方式：
//
//   - "动手之前先读正文"——否则模型就照着描述行动，而描述只有一行，
//     写它是为了让人挑得出来，不是为了够用。
//   - "最多读一条"——给模型五个看着都像的技能，它会五个全读，于是省
//     下来的 token 变成多花的 token 外加五趟往返。
//   - "一条都不合适就别理"——没有这句，技能列表读起来就像一份非点不
//     可的菜单，而模型总能找出一条差不多沾边的。
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

// skillsCost 报的是索引要花多少，以及全部加载进来又要花多少。
//
// 值得打出来，因为索引**不是**免费的，而这道算术就是整个设计决定。
// 每个技能的名称和描述，会在会话全程待在每次请求的前缀里。四十个技
// 能就是两千来 token 的永久开销——虽然进了缓存，阶段 04 之后只按十
// 分之一计价，但永远不是零。技能目录没人修剪地长下去，就是向 Agent
// 此后每一次调用征的税，而唯一能让人察觉的办法，是有东西把这个数字
// 打出来。
func skillsCost(skills []skill) (indexBytes, bodyBytes int) {
	indexBytes = len(skillsPrompt(skills))
	for _, s := range skills {
		bodyBytes += s.BodyBytes
	}
	return indexBytes, bodyBytes
}
