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
			if !strings.Contains(string(raw), "$URL") {
				t.Errorf("%s: the http entry must carry $URL: %s", h.Name, raw)
			}
			// One of the two spellings of the credential, never neither: an
			// entry with no token at all is a server the harness cannot
			// authenticate to, written silently.
			if !strings.Contains(string(raw), "$KEY") {
				t.Errorf("%s: the http entry carries no $KEY or $KEYENV: %s", h.Name, raw)
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

// Every row says where its shape came from, and when.
//
// This is the only check a repository CAN make about a third party's config
// format: nothing here can open Windsurf and see whether it really reads
// `serverUrl`. What it can refuse is a row added on a guess, which is how the
// first version of this table got two rows wrong (Codex's hosted entry used a
// headers map it does not read, and Zed was marked as having no hosted form
// at all). A citation makes the claim checkable by a person and makes a stale
// one visible.
func TestEveryHarnessCitesItsSource(t *testing.T) {
	for _, h := range Harnesses() {
		if len(h.Source) < 10 {
			t.Errorf("%s has no source: say which vendor doc or which command confirmed this shape", h.Name)
		}
		if !dateLike(h.Checked) {
			t.Errorf("%s: checked = %q, want a YYYY-MM-DD date", h.Name, h.Checked)
		}
	}
}

// NeedsKeyValue tells a harness that embeds the token from one that is handed
// the variable's name, and $KEY being a prefix of $KEYENV is exactly the trap
// it exists to avoid.
func TestNeedsKeyValue(t *testing.T) {
	codex, ok := FindHarness("codex")
	if !ok {
		t.Fatal("codex is missing")
	}
	if codex.NeedsKeyValue() {
		t.Error("codex reads the token from the environment, so no stored key is required")
	}

	claude, ok := FindHarness("claude-code")
	if !ok {
		t.Fatal("claude-code is missing")
	}
	if !claude.NeedsKeyValue() {
		t.Error("claude-code embeds the token in its config, so a key is required")
	}

	// A harness with no hosted form needs nothing.
	if (Harness{}).NeedsKeyValue() {
		t.Error("an empty harness asked for a key")
	}
}

func dateLike(s string) bool {
	if len(s) != len("2006-01-02") || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
