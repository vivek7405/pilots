package agents

import (
	"encoding/json"
	"strings"
	"testing"
)

// The table is read by the CLI and the website; a malformed row would only
// surface when a user typed its name.
func TestHarnessesTable(t *testing.T) {
	hs := Harnesses()
	if len(hs) < 8 {
		t.Fatalf("only %d harnesses", len(hs))
	}
	seen := map[string]bool{}
	for _, h := range hs {
		if h.Name == "" || h.Title == "" {
			t.Errorf("a harness has no name or title: %+v", h)
		}
		if seen[h.Name] {
			t.Errorf("%s is listed twice", h.Name)
		}
		seen[h.Name] = true
		if h.Format != "json" && h.Format != "toml" {
			t.Errorf("%s: format %q", h.Name, h.Format)
		}
		if len(h.Path) != 2 || h.Path[1] != "pilots" {
			t.Errorf("%s: the entry must sit at <servers key>.pilots, got %v", h.Name, h.Path)
		}
		if h.User == "" && h.Project == "" {
			t.Errorf("%s: no config file for either scope", h.Name)
		}
		if h.HTTP == nil && h.Stdio == nil {
			t.Errorf("%s: no transport", h.Name)
		}
		if h.HTTP != nil {
			raw, _ := json.Marshal(h.HTTP)
			if !strings.Contains(string(raw), "$URL") || !strings.Contains(string(raw), "$KEY") {
				t.Errorf("%s: the http entry must carry $URL and $KEY: %s", h.Name, raw)
			}
		}
		if h.Stdio != nil {
			raw, _ := json.Marshal(h.Stdio)
			if !strings.Contains(string(raw), `"pilot"`) || !strings.Contains(string(raw), `"mcp"`) {
				t.Errorf("%s: the stdio entry must run `pilot mcp`: %s", h.Name, raw)
			}
		}
	}
	for _, name := range []string{"claude-code", "codex", "cursor", "opencode", "generic"} {
		if !seen[name] {
			t.Errorf("%s is missing from the table", name)
		}
	}
	if _, ok := FindHarness("no-such-harness"); ok {
		t.Error("an unknown name resolved")
	}
}
