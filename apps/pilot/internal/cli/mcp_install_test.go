package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/agents"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

func installEnv(t *testing.T) (*Env, config.Env, string) {
	t.Helper()
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	env := &Env{W: out.New(false),
		APIURL: config.Resolved{Value: "https://api.example.test"},
		APIKey: config.Resolved{Value: "pilot_k3y"},
	}
	return env, getenv, home
}

// Every harness round-trips: the hosted entry lands at its key path with the
// URL and the key filled in, a second run changes nothing, and --stdio
// replaces it rather than adding a second entry.
func TestInstallEveryHarness(t *testing.T) {
	env, getenv, home := installEnv(t)
	for _, h := range agents.Harnesses() {
		if h.User == "" {
			continue
		}
		t.Run(h.Name, func(t *testing.T) {
			path, err := harnessConfigPath(getenv, h, false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(path, home) {
				t.Fatalf("path %s is outside HOME", path)
			}
			stdio := h.HTTP == nil
			entry, err := serverEntry(env, h, stdio)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := writeEntry(path, h, entry)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("first write reported no change")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !stdio {
				if !strings.Contains(string(raw), "https://api.example.test/mcp") || !strings.Contains(string(raw), "Bearer pilot_k3y") {
					t.Fatalf("the hosted entry lost the URL or the key:\n%s", raw)
				}
			}
			if h.Format == "json" {
				var cfg map[string]any
				if err := json.Unmarshal(raw, &cfg); err != nil {
					t.Fatalf("%s is not JSON: %v", path, err)
				}
				servers, _ := cfg[h.Path[0]].(map[string]any)
				if servers == nil || servers[h.Path[1]] == nil {
					t.Fatalf("no entry at %v in %s", h.Path, raw)
				}
			} else if !strings.Contains(string(raw), "["+strings.Join(h.Path, ".")+"]") {
				t.Fatalf("no table in %s", raw)
			}
			changed, err = writeEntry(path, h, entry)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Fatal("a second identical write reported a change")
			}
			if h.Stdio != nil && h.HTTP != nil {
				other, err := serverEntry(env, h, true)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err = writeEntry(path, h, other); err != nil || !changed {
					t.Fatalf("switching to stdio: changed=%v err=%v", changed, err)
				}
				raw, _ = os.ReadFile(path)
				if strings.Contains(string(raw), "Bearer pilot_k3y") {
					t.Fatalf("the key survived a switch to stdio:\n%s", raw)
				}
				if strings.Count(string(raw), "pilots") == 0 {
					t.Fatalf("the entry vanished:\n%s", raw)
				}
			}
			if st, err := os.Stat(path); err == nil && st.Mode().Perm() != 0o600 {
				t.Errorf("%s is %v; a file carrying a key must be 0600", path, st.Mode().Perm())
			}
		})
	}
}

// Another server's entry and unrelated keys survive the merge; a file that
// is not JSON is refused rather than overwritten.
func TestInstallKeepsWhatIsThere(t *testing.T) {
	env, getenv, _ := installEnv(t)
	h, _ := agents.FindHarness("cursor")
	path, _ := harnessConfigPath(getenv, h, false)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"other":{"command":"x"}},"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, _ := serverEntry(env, h, false)
	if _, err := writeEntry(path, h, entry); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["pilots"] == nil || cfg["theme"] != "dark" {
		t.Fatalf("the merge lost something: %s", raw)
	}

	if err := os.WriteFile(path, []byte(`{ not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeEntry(path, h, entry); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("a broken file must be refused, got %v", err)
	}
}

// The TOML writer replaces its own table in place and keeps every other line.
func TestInstallCodexTOML(t *testing.T) {
	env, getenv, _ := installEnv(t)
	h, _ := agents.FindHarness("codex")
	path, _ := harnessConfigPath(getenv, h, false)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	before := "model = \"o3\"\n\n[mcp_servers.other]\ncommand = \"x\"\n\n[mcp_servers.pilots]\ncommand = \"old\"\n\n[features]\nfoo = true\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, _ := serverEntry(env, h, false)
	if _, err := writeEntry(path, h, entry); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	for _, want := range []string{"model = \"o3\"", "[mcp_servers.other]", "command = \"x\"", "[features]", "foo = true",
		"[mcp_servers.pilots]", `url = "https://api.example.test/mcp"`, `http_headers = { Authorization = "Bearer pilot_k3y" }`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `command = "old"`) {
		t.Errorf("the old table survived:\n%s", got)
	}
	if strings.Count(got, "[mcp_servers.pilots]") != 1 {
		t.Errorf("the table is not exactly once:\n%s", got)
	}
}

func TestInstallRefusals(t *testing.T) {
	env, _, _ := installEnv(t)
	zed, _ := agents.FindHarness("zed")
	if _, err := serverEntry(env, zed, false); err == nil || !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("zed must be refused the hosted form with the stdio hint, got %v", err)
	}
	cc, _ := agents.FindHarness("claude-code")
	env.APIKey.Value = ""
	if _, err := serverEntry(env, cc, false); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("no key must be refused, got %v", err)
	}
}

func TestPrintRendersThePastableEntry(t *testing.T) {
	env, _, _ := installEnv(t)
	cc, _ := agents.FindHarness("claude-code")
	entry, _ := serverEntry(env, cc, false)
	snippet, err := renderEntry(cc, entry)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(snippet), &doc); err != nil {
		t.Fatalf("not JSON: %s", snippet)
	}
	if doc["mcpServers"]["pilots"]["url"] != "https://api.example.test/mcp" {
		t.Fatalf("wrong url in %s", snippet)
	}
	codex, _ := agents.FindHarness("codex")
	entry, _ = serverEntry(env, codex, true)
	snippet, _ = renderEntry(codex, entry)
	if !strings.HasPrefix(snippet, "[mcp_servers.pilots]\n") || !strings.Contains(snippet, `args = ["mcp"]`) {
		t.Fatalf("codex stdio snippet: %s", snippet)
	}
}

func TestDoctorNamesRegisteredHarnesses(t *testing.T) {
	env, getenv, _ := installEnv(t)
	if c := checkHarnesses(getenv); !c.OK || !strings.Contains(c.Fix, "pilot mcp install") {
		t.Fatalf("empty home: %+v", c)
	}
	for _, name := range []string{"claude-code", "codex"} {
		h, _ := agents.FindHarness(name)
		path, _ := harnessConfigPath(getenv, h, false)
		entry, _ := serverEntry(env, h, false)
		if _, err := writeEntry(path, h, entry); err != nil {
			t.Fatal(err)
		}
	}
	c := checkHarnesses(getenv)
	if !strings.Contains(c.Detail, "claude-code") || !strings.Contains(c.Detail, "codex") {
		t.Fatalf("doctor missed a registration: %+v", c)
	}
}
