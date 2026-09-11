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
				if !strings.Contains(string(raw), "https://api.example.test/mcp") {
					t.Fatalf("the hosted entry lost the URL:\n%s", raw)
				}
				// Two ways to carry the credential, and which one a harness
				// gets is not a style choice: Codex is handed the variable's
				// NAME and reads it at runtime, so its file holds no secret.
				if h.NeedsKeyValue() {
					if !strings.Contains(string(raw), "Bearer pilot_k3y") {
						t.Fatalf("the hosted entry lost the key:\n%s", raw)
					}
				} else {
					if strings.Contains(string(raw), "pilot_k3y") {
						t.Fatalf("%s reads the token from the environment; its file must not contain it:\n%s", h.Name, raw)
					}
					if !strings.Contains(string(raw), "PILOT_API_KEY") {
						t.Fatalf("the hosted entry names no environment variable:\n%s", raw)
					}
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

	// A config that was already 0644 must come back 0600, not stay readable
	// by everyone on the box. The atomic write preserves the existing mode so
	// it cannot silently tighten somebody's deliberate 0400 -- but preserving
	// it UNMASKED left a live bearer token world-readable, and the perm
	// assertion in TestInstallEveryHarness never saw it because the file
	// there does not pre-exist.
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("a pre-existing 0644 config carrying a key is %v, want 0600", st.Mode().Perm())
	}
	if !strings.Contains(string(raw), "pilot_k3y") {
		t.Fatal("the test is not actually proving anything: no key was written")
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
		"[mcp_servers.pilots]", `url = "https://api.example.test/mcp"`, `bearer_token_env_var = "PILOT_API_KEY"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The shape above is what `codex mcp add --url ... --bearer-token-env-var ...`
	// writes, checked against codex-cli 0.153.0. The earlier version of this
	// row used an `http_headers` table Codex does not read, which is the bug
	// that made every hosted Codex install silently unauthenticated.
	if strings.Contains(got, "pilot_k3y") {
		t.Errorf("the token was written into config.toml; Codex reads it from the environment:\n%s", got)
	}
	if strings.Contains(got, "http_headers") {
		t.Errorf("http_headers is not a key Codex reads:\n%s", got)
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
	// Claude Desktop takes a local command and nothing else, so the hosted
	// form is refused and the refusal names the flag that works. Zed is NOT
	// this case: it reads a remote `url` with `headers`, which the first
	// version of the table missed.
	desktop, _ := agents.FindHarness("claude-desktop")
	if _, err := serverEntry(env, desktop, false); err == nil || !strings.Contains(err.Error(), "stdio") {
		t.Fatalf("claude-desktop must be refused the hosted form with the stdio hint, got %v", err)
	}
	zed, _ := agents.FindHarness("zed")
	if _, err := serverEntry(env, zed, false); err != nil {
		t.Fatalf("zed supports a remote server, so the hosted form must work: %v", err)
	}
	cc, _ := agents.FindHarness("claude-code")
	env.APIKey.Value = ""
	if _, err := serverEntry(env, cc, false); err == nil || !strings.Contains(err.Error(), "no API key") {
		t.Fatalf("no key must be refused, got %v", err)
	}
	// But a harness that is handed the variable's NAME needs no stored key:
	// demanding one would refuse a setup that works, and would be asking for
	// a secret in order to not write it down.
	codex, _ := agents.FindHarness("codex")
	entry, err := serverEntry(env, codex, false)
	if err != nil {
		t.Fatalf("codex needs no stored key, got %v", err)
	}
	if entry["bearer_token_env_var"] != "PILOT_API_KEY" {
		t.Fatalf("codex entry = %+v", entry)
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
