package agents

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPagesStartWithSkillMD(t *testing.T) {
	pages := Pages()
	if len(pages) < 2 {
		t.Fatalf("expected SKILL.md and references, got %d pages", len(pages))
	}
	if pages[0].Name != "SKILL.md" {
		t.Fatalf("first page is %q, want SKILL.md", pages[0].Name)
	}
	if !strings.HasPrefix(pages[0].Body, "---\nname: pilots") {
		t.Fatalf("SKILL.md has no frontmatter naming the skill: %q", firstLine(pages[0].Body))
	}
	for _, p := range pages[1:] {
		if !strings.HasPrefix(p.Name, "references/") {
			t.Errorf("page %q is neither SKILL.md nor a reference", p.Name)
		}
		// A page an agent loads into its context has to stay short enough to
		// be read whole; the budget is the same one the primer has.
		if n := strings.Count(p.Body, "\n"); n > 400 {
			t.Errorf("%s is %d lines; a reference page must stay under 400", p.Name, n)
		}
	}
}

func TestTopicsMatchPages(t *testing.T) {
	topics := Topics()
	if len(topics) == 0 {
		t.Fatal("no topics")
	}
	for _, name := range topics {
		if _, ok := Topic(name); !ok {
			t.Errorf("topic %q listed but unreadable", name)
		}
	}
	if _, ok := Topic("no-such-page"); ok {
		t.Error("an unknown topic resolved")
	}
}

// The plugin manifests. Nothing in Go reads them, so nothing else would notice
// a stray comma until a user ran `/plugin install`.
func TestPluginManifestsParse(t *testing.T) {
	for _, path := range []string{
		".claude-plugin/plugin.json", ".mcp.json", "hooks/hooks.json",
		"plugin.json", "mcp.json", "../.claude-plugin/marketplace.json",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// Both MCP manifests name the same server, and the hook matches it: a rename
// in one file that misses the other would leave the guard matching nothing,
// which is the failure mode a guard must never have quietly.
func TestHookMatchesTheDeclaredServer(t *testing.T) {
	servers := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Servers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v.Servers
	}
	claude, portable := servers(".mcp.json"), servers("mcp.json")
	if _, ok := claude["pilots"]; !ok {
		t.Fatal(".mcp.json does not declare the pilots server")
	}
	if _, ok := portable["pilots"]; !ok {
		t.Fatal("mcp.json does not declare the pilots server")
	}
	raw, err := os.ReadFile("hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var hooks struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatal(err)
	}
	if len(hooks.Hooks.PreToolUse) == 0 {
		t.Fatal("no PreToolUse hook")
	}
	re := regexp.MustCompile(hooks.Hooks.PreToolUse[0].Matcher)
	for _, name := range []string{"mcp__plugin_pilots_pilots__destroy_machine", "mcp__pilots__exec"} {
		if !re.MatchString(name) {
			t.Errorf("the hook matcher does not match %s", name)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
