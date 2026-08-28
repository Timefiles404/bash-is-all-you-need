// Stage 07 — skills, which are a directory and one paragraph.
//
// "Skills" sounds like a subsystem: a registry, a loader, a matcher, probably an
// embedding. It is none of those. A skill is:
//
//	a Markdown file, plus a sentence telling the model it exists.
//
// The model reads the body when it decides the skill applies, using `cat`. There
// is no skill tool, no retrieval step, and no runtime — which is the same
// observation stage 05 made about memory, arriving from a different direction.
// Once the agent has a shell, "load this document when relevant" is not a
// feature you build. It is a filename.
//
// What is genuinely load-bearing is the *shape*: name and description always in
// context, body only on demand. That is **progressive disclosure**, and it is
// the only reason a project can have forty skills without a forty-skill prompt.
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
	Path        string // relative, because the model has to be able to cat it
	BodyBytes   int    // what it would cost to load, for the accounting
}

// loadSkills scans skills/<name>/SKILL.md.
//
// A directory per skill rather than a flat file per skill, because a real skill
// grows attachments — a script it tells the model to run, a template, an
// example input. Those live next to it, and the model can find them with `ls`
// because it already knows the directory.
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
			// A skill with no description is invisible: the index is the only
			// thing the model sees, so a missing description means the skill
			// will never be chosen. Skipping it silently would hide that;
			// naming it in the index with an explicit complaint would put the
			// complaint in every request forever. Skip, and let the count in
			// the skills_indexed event not match the directory listing.
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

// parseFrontmatter reads `name:` and `description:` out of a leading `---`
// block.
//
// Twenty lines instead of a YAML dependency, and the trade is worth stating
// because it is the same trade the whole repo makes. YAML would handle nested
// values, anchors, multi-line scalars and type coercion — none of which two
// string fields need. What it would cost is a dependency in a project whose
// argument is that you can read all of it, to parse a file format we also
// control. When you own both ends of an interface, the parser is allowed to be
// as small as the interface.
//
// The failure mode is honest: anything this does not understand is ignored, and
// a skill with no description does not appear.
func parseFrontmatter(s string) (name, description string) {
	// A skill file authored on Windows very often starts with a UTF-8 BOM, and a
	// literal U+FEFF is a compile error anywhere but byte zero of a Go source file,
	// so the cutset is spelled with rune values: BOM, space, tab, CR, LF.
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

// skillsPrompt renders the index that goes into the system prompt.
//
// Three instructions, and each one exists because of a way this goes wrong:
//
//   - "read the body before acting on it" — otherwise the model acts on the
//     description, which is one line long and was written to be selectable, not
//     to be sufficient.
//   - "at most one" — a model given five plausible skills will read all five,
//     which converts a token saving into a token cost plus five round trips.
//   - "if none applies, ignore them" — without it, a skills list reads as a menu
//     the model is expected to order from, and it will find one that nearly fits.
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

// skillsCost reports what the index costs and what loading everything would.
//
// Worth printing, because the index is NOT free and the arithmetic is the whole
// design decision. Every skill's name and description sit in the prefix of every
// request for the life of the session. Forty skills is a couple of thousand
// tokens of permanent overhead — cached, at a tenth of the price after stage 04,
// but never zero. A skills directory that grows without anyone pruning it is a
// tax levied on every call the agent ever makes, and the only way anyone notices
// is if something prints the number.
func skillsCost(skills []skill) (indexBytes, bodyBytes int) {
	indexBytes = len(skillsPrompt(skills))
	for _, s := range skills {
		bodyBytes += s.BodyBytes
	}
	return indexBytes, bodyBytes
}
