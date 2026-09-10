package agents

import (
	_ "embed"
	"encoding/json"
)

//go:embed harnesses.json
var harnessesJSON []byte

// Harness is one coding agent that can load the pilots MCP server: where its
// config lives and what the server entry looks like there. The table is
// harnesses.json, so the CLI (`pilot mcp install`) and the website's
// integrations page read one list.
type Harness struct {
	// Name is what `pilot mcp install <name>` takes.
	Name  string `json:"name"`
	Title string `json:"title"`
	// User and Project are the config files for each scope; "~" is HOME. A
	// harness with no Project entry has no per-repository form, and one with
	// no User entry has no global one.
	User       string `json:"user,omitempty"`
	UserDarwin string `json:"user_darwin,omitempty"`
	Project    string `json:"project,omitempty"`
	// Format is "json" or "toml".
	Format string `json:"format"`
	// Path is where the server entry sits in the file, e.g. mcpServers.pilots.
	Path []string `json:"path"`
	// HTTP and Stdio are the entry for each transport, with "$URL" and "$KEY"
	// to be substituted. A harness that cannot dial a URL has no HTTP entry.
	HTTP  map[string]any `json:"http,omitempty"`
	Stdio map[string]any `json:"stdio,omitempty"`
	// After is printed once the file is written: the plugin lines, a
	// restart, whatever the harness needs next.
	After []string `json:"after,omitempty"`
	// Verify is the one-line check that it worked.
	Verify string `json:"verify,omitempty"`
}

// Harnesses is the table, in the order it is documented.
func Harnesses() []Harness {
	var out []Harness
	if err := json.Unmarshal(harnessesJSON, &out); err != nil {
		panic("agents/harnesses.json: " + err.Error())
	}
	return out
}

// FindHarness looks one up by name.
func FindHarness(name string) (Harness, bool) {
	for _, h := range Harnesses() {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}
