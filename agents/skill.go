// Package agents is the agent-facing package of pilots: the skill pages an
// agent reads, embedded so that every binary that serves them (hostd's /mcp
// and `pilot mcp`) carries the same copy the plugin in this directory ships.
//
// The directory doubles as the Claude Code plugin root and the portable Agent
// Plugins v1 package, which is why the pages live here rather than beside the
// CLI: a plugin's components must sit inside the plugin directory, and an
// embedded file must sit inside its Go module. One directory satisfies both,
// so the pages exist once.
package agents

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

// Skill holds skills/pilots: SKILL.md and references/*.md.
//
//go:embed skills/pilots
var Skill embed.FS

// Page is one file of the skill.
type Page struct {
	// Name is the page's path inside the skill: SKILL.md, or references/deploy.md.
	Name string
	Body string
}

// Pages lists the skill in reading order: SKILL.md first, then the references
// sorted by name.
func Pages() []Page {
	root, err := fs.Sub(Skill, "skills/pilots")
	if err != nil {
		return nil
	}
	pages := []Page{}
	if body, err := fs.ReadFile(root, "SKILL.md"); err == nil {
		pages = append(pages, Page{Name: "SKILL.md", Body: string(body)})
	}
	entries, err := fs.ReadDir(root, "references")
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
		body, err := fs.ReadFile(root, "references/"+n)
		if err != nil {
			continue
		}
		pages = append(pages, Page{Name: "references/" + n, Body: string(body)})
	}
	return pages
}

// Topics is what the `docs` tool accepts: every reference page's name without
// its directory and extension, derived from the pages rather than listed
// twice.
func Topics() []string {
	out := []string{}
	for _, p := range Pages() {
		if strings.HasPrefix(p.Name, "references/") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(p.Name, "references/"), ".md"))
		}
	}
	return out
}

// Topic returns one reference page's text.
func Topic(name string) (string, bool) {
	for _, p := range Pages() {
		if p.Name == "references/"+name+".md" {
			return p.Body, true
		}
	}
	return "", false
}
