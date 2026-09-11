package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/agents"

	"github.com/vivek7405/pilots/cli/internal/out"
)

// Zed and VS Code write JSONC. A settings file with comments in it is a
// perfectly good file, and the old refusal -- "is not valid JSON; fix it or
// move it aside" -- read as "delete your settings" to the person whose
// settings were fine.
func TestAConfigWithCommentsIsRefusedByName(t *testing.T) {
	env, getenv, _ := installEnv(t)
	h, _ := agents.FindHarness("zed")
	path, _ := harnessConfigPath(getenv, h, false)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shape Zed ships: a comment header over real settings.
	body := "// Zed settings\n//\n// For information on how to configure Zed, see\n" +
		"// the Zed documentation: https://zed.dev/docs/configuring-zed\n" +
		"{\n  \"theme\": \"One Dark\",\n  \"context_servers\": {}\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	entry, err := serverEntry(env, h, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writeEntry(path, h, entry)
	if err == nil {
		t.Fatal("a commented config was rewritten; the comments would be gone")
	}
	if !strings.Contains(err.Error(), "comments") {
		t.Errorf("the refusal does not say what is actually wrong: %v", err)
	}
	// The actionable half is the Next field, which the CLI renders as the
	// "→" line under the error. Asserted there rather than on Error(),
	// because that is where the reader actually sees it.
	var f *out.Failure
	if !errors.As(err, &f) {
		t.Fatalf("the refusal is not a *out.Failure, so it carries no next step: %v", err)
	}
	if !strings.Contains(f.Next, "zed --print") {
		t.Errorf("the next step does not name the command that works: %q", f.Next)
	}

	// And the file is untouched. Declining is only the better outcome if
	// nothing was written.
	after, _ := os.ReadFile(path)
	if string(after) != body {
		t.Errorf("the file was modified despite the refusal:\n%s", after)
	}
}

// A genuinely broken file still gets the old message: it is not JSONC, and
// telling someone to paste an entry into a file that does not parse at all
// would send them in a circle.
func TestABrokenConfigStillSaysItIsNotJSON(t *testing.T) {
	env, getenv, _ := installEnv(t)
	h, _ := agents.FindHarness("cursor")
	path, _ := harnessConfigPath(getenv, h, false)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{ not json at all`), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, _ := serverEntry(env, h, false)
	_, err := writeEntry(path, h, entry)
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want the not-valid-JSON refusal, got %v", err)
	}
	if strings.Contains(err.Error(), "comments") {
		t.Errorf("a broken file was blamed on comments: %v", err)
	}
}

// The stripper exists only to RECOGNISE a JSONC file, so the bar is that it
// never mistakes string content for a comment. A config full of URLs is the
// case that breaks a regexp, and every one of these is a real value someone
// has in an MCP config.
func TestStripJSONCommentsLeavesStringsAlone(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]any
	}{
		{
			"a url is not a line comment",
			`{"url":"https://api.example.test/mcp"}`,
			map[string]any{"url": "https://api.example.test/mcp"},
		},
		{
			"a line comment goes",
			"// header\n{\"a\":1}",
			map[string]any{"a": float64(1)},
		},
		{
			"a trailing comment goes",
			"{\"a\":1} // why\n",
			map[string]any{"a": float64(1)},
		},
		{
			"a block comment goes",
			"/* a\n   b */{\"a\":1}",
			map[string]any{"a": float64(1)},
		},
		{
			"a comment marker inside a string stays",
			`{"note":"see /* this */ and // that"}`,
			map[string]any{"note": "see /* this */ and // that"},
		},
		{
			"an escaped quote does not end the string",
			`{"note":"a \" then // not a comment"}`,
			map[string]any{"note": `a " then // not a comment`},
		},
		{
			"a windows path's backslashes survive",
			`{"command":"C:\\tools\\pilot.exe"}`,
			map[string]any{"command": `C:\tools\pilot.exe`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			stripped := stripJSONComments([]byte(c.in))
			if err := json.Unmarshal(stripped, &got); err != nil {
				t.Fatalf("stripped output does not parse: %v\n%s", err, stripped)
			}
			for k, want := range c.want {
				if got[k] != want {
					t.Errorf("%s = %#v, want %#v", k, got[k], want)
				}
			}
		})
	}
}
