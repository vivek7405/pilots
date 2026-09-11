package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/agents"

	"github.com/vivek7405/pilots/cli/internal/config"
)

// The skill: the same pages are three things at once -- files an agent reads
// off disk after `pilot init`, MCP resources under pilots-docs://, and the
// `docs` tool's answers. One copy serves all three, so a fix to a page cannot
// land in one surface and miss the other two.
//
// The copy is the one embedded from agents/skills/pilots, which every build
// of this binary carries. A copy ON DISK wins over it, in this order: a
// repository's own .agents/skills/pilots (walking up from the working
// directory; a team that edited it meant to), PILOT_SKILL_DIR, the checkout
// this binary was built in, and ~/.local/share/pilots/skill, which
// `pilot skill install` writes. skillRoot returns "" when none of those
// exist, and loadSkill answers with the embedded pages then.
func skillRoot(getenv config.Env) string {
	if dir, err := os.Getwd(); err == nil {
		for {
			if hasSkill(filepath.Join(dir, ".agents", "skills", "pilots")) {
				return filepath.Join(dir, ".agents", "skills", "pilots")
			}
			// Working inside the pilots checkout itself, wherever the binary
			// was built.
			if hasSkill(filepath.Join(dir, "agents", "skills", "pilots")) {
				return filepath.Join(dir, "agents", "skills", "pilots")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if d := getenv("PILOT_SKILL_DIR"); d != "" && hasSkill(d) {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			// A binary built anywhere inside a checkout finds the checkout's
			// pages by walking up to the repository root.
			for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
				if d := filepath.Join(dir, "agents", "skills", "pilots"); hasSkill(d) {
					return d
				}
				if filepath.Dir(dir) == dir {
					break
				}
			}
		}
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if d := filepath.Join(home, ".local", "share", "pilots", "skill"); hasSkill(d) {
		return d
	}
	return ""
}

func hasSkill(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !st.IsDir()
}

// loadSkill reads the pages from a root on disk, or answers the embedded copy
// when root is empty: SKILL.md first, then the references, sorted.
func loadSkill(root string) []agents.Page {
	if root == "" {
		return agents.Pages()
	}
	pages := []agents.Page{}
	if raw, err := os.ReadFile(filepath.Join(root, "SKILL.md")); err == nil {
		pages = append(pages, agents.Page{Name: "SKILL.md", Body: string(raw)})
	}
	refs := filepath.Join(root, "references")
	entries, err := os.ReadDir(refs)
	if err != nil {
		return pages
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(refs, n))
		if err != nil {
			continue
		}
		pages = append(pages, agents.Page{Name: "references/" + n, Body: string(raw)})
	}
	return pages
}

// writeSkill materialises pages under target, the layout `pilot init` copies
// into a repository and `pilot skill install` keeps in ~/.local/share.
func writeSkill(pages []agents.Page, target string) error {
	for _, p := range pages {
		dst := filepath.Join(target, filepath.FromSlash(p.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(p.Body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// The stanza `pilot init` appends to AGENTS.md, matched by its first line
// on a re-run.
const agentsHeading = "## Deploying with pilots"

const agentsStanza = agentsHeading + `

This repository deploys to pilots. The skill is at ` + "`.agents/skills/pilots/`" + `;
read ` + "`SKILL.md`" + ` before deploying, and load at most two reference pages.

The one call is ` + "`pilot deploy`" + ` in the app's directory, or the MCP ` + "`deploy`" + `
tool with ` + "`dir`" + `. The platform decides what the directory is. Do not write a
Dockerfile or a compose file first; write one only when the answer is
` + "`unknown_framework`" + `, and obey the two rules its ` + "`details.rules`" + ` names.

Every error carries ` + "`code`" + `, ` + "`next`" + ` and ` + "`details`" + `. Read ` + "`next`" + ` and do that.
`
