package agents

import (
	_ "embed"
	"encoding/json"
	"strings"
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
	// HTTP and Stdio are the entry for each transport. Three placeholders are
	// substituted: "$URL" is the fleet's /mcp endpoint, "$KEY" is the token
	// itself, and "$KEYENV" is the NAME of the environment variable holding
	// it. A harness that cannot dial a URL has no HTTP entry.
	//
	// $KEYENV rather than $KEY is how a harness that reads the token at
	// runtime is spelled, and it is the better of the two: the config file
	// then carries no secret. Codex is the one that works this way.
	HTTP  map[string]any `json:"http,omitempty"`
	Stdio map[string]any `json:"stdio,omitempty"`
	// After is printed once the file is written: the plugin lines, a
	// restart, whatever the harness needs next.
	After []string `json:"after,omitempty"`
	// Verify is the one-line check that it worked.
	Verify string `json:"verify,omitempty"`
	// Source is where this row's shape was confirmed: a vendor URL, or the
	// command that was run against the product itself. Required.
	//
	// It exists because nothing in CI can check a third party's file format.
	// A row is a CLAIM about somebody else's software, and the most a
	// repository can do about a claim is say where it came from and when, so
	// a reader can check it and a stale one is visible rather than silent.
	Source string `json:"source"`
	// Checked is the date Source was last confirmed, YYYY-MM-DD.
	Checked string `json:"checked"`
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

// KeyEnvVar is the environment variable a harness is pointed at when it reads
// the token itself rather than having it written into its config file. The
// same name every other surface uses, so one export serves the CLI, the SDKs
// and the harness.
const KeyEnvVar = "PILOT_API_KEY"

// NeedsKeyValue reports whether this harness's hosted entry embeds the token
// itself, which is what decides whether `pilot mcp install` has to have one
// on hand. A harness that takes only the variable's NAME needs no stored key,
// and demanding one would refuse a setup that works.
//
// $KEYENV is masked before the search rather than matched around, because
// "$KEY" is a prefix of "$KEYENV" and a substring test for the shorter one
// finds the longer one every time.
func (h Harness) NeedsKeyValue() bool {
	raw, err := json.Marshal(h.HTTP)
	if err != nil {
		// A row that will not marshal is a broken row; demanding the key is
		// the conservative half of a choice that should never be reached.
		return true
	}
	return strings.Contains(strings.ReplaceAll(string(raw), "$KEYENV", ""), "$KEY")
}
