package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/agents"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

// `pilot mcp install <harness>`: register the pilots MCP server with a coding
// agent, in that agent's own config file, the way it wants the entry shaped.
//
// The default is the HOSTED server, https://<api>/mcp with the key `pilot
// login` stored, because it needs nothing on PATH and works from any host of
// the fleet. --stdio registers `pilot mcp` instead, which adds the six tools
// that read this machine's filesystem (deploy a directory, push a file).
//
// The table of harnesses is agents/harnesses.json, shared with the website,
// so a harness documented there is installable here and vice versa.
func newMCPInstallCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		stdio   bool
		project bool
		print   bool
		list    bool
	)
	c := &cobra.Command{
		Use:   "install [harness]",
		Short: "register the pilots MCP server with a coding agent",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if list || len(args) == 0 {
				return listHarnesses(env)
			}
			h, ok := agents.FindHarness(args[0])
			if !ok {
				names := make([]string, 0)
				for _, x := range agents.Harnesses() {
					names = append(names, x.Name)
				}
				return out.Failf("pilot mcp install --list", "no harness %s; the names are %s", args[0], strings.Join(names, ", "))
			}
			entry, err := serverEntry(env, h, stdio)
			if err != nil {
				return err
			}
			path, err := harnessConfigPath(getenv, h, project)
			if err != nil {
				return err
			}
			if print {
				snippet, err := renderEntry(h, entry)
				if err != nil {
					return err
				}
				if env.W.JSON {
					return env.W.JSONValue(map[string]any{"harness": h.Name, "path": path, "entry": entry})
				}
				env.W.Linef("# %s", path)
				env.W.Linef("%s", snippet)
				return nil
			}
			changed, err := writeEntry(path, h, entry)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"harness": h.Name, "path": path, "changed": changed, "transport": transportName(stdio)})
			}
			if changed {
				env.W.Notef("wrote %s (%s)", path, transportName(stdio))
			} else {
				env.W.Notef("kept %s (already registered, %s)", path, transportName(stdio))
			}
			for _, line := range h.After {
				env.W.Notef("%s", line)
			}
			if h.Verify != "" {
				env.W.Notef("verify: %s", h.Verify)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&stdio, "stdio", false, "register `pilot mcp` on stdio instead of the hosted https://<api>/mcp")
	c.Flags().BoolVar(&project, "project", false, "write the repository-scoped file in the current directory instead of the user-wide one")
	c.Flags().BoolVar(&print, "print", false, "print the entry instead of writing it")
	c.Flags().BoolVar(&list, "list", false, "list the harnesses and their config files")
	Describe(c, Doc{
		What: "One command per coding agent: Claude Code, Codex, Cursor, OpenCode,\n" +
			"Windsurf, Gemini CLI, VS Code, Zed, Claude Desktop, or `generic` for\n" +
			"an mcp.json any client reads. The entry goes into the agent's own\n" +
			"config file, merged with what is there.",
		How: "Hosted by default: the fleet's /mcp with the key from `pilot login`,\n" +
			"nothing to run locally. --stdio registers `pilot mcp` instead, which\n" +
			"adds deploy, build, plan, generate_dockerfile, push_file and pull_file:\n" +
			"the tools that read this machine's files. Run it again to switch.",
		Examples: []string{
			"pilot mcp install claude-code",
			"pilot mcp install codex --stdio",
			"pilot mcp install cursor --project",
			"pilot mcp install opencode --print",
			"pilot mcp install --list",
		},
		Related: []string{
			"pilot init      the per-repository setup: skill pages, .mcp.json, AGENTS.md",
			"pilot mcp       what the stdio entry runs",
		},
	})
	return c
}

func transportName(stdio bool) string {
	if stdio {
		return "stdio"
	}
	return "hosted"
}

func listHarnesses(env *Env) error {
	hs := agents.Harnesses()
	if env.W.JSON {
		return env.W.JSONValue(hs)
	}
	rows := make([][]string, 0, len(hs))
	for _, h := range hs {
		transports := []string{}
		if h.HTTP != nil {
			transports = append(transports, "hosted")
		}
		if h.Stdio != nil {
			transports = append(transports, "stdio")
		}
		user, project := h.User, h.Project
		if user == "" {
			user = "-"
		}
		if project == "" {
			project = "-"
		}
		rows = append(rows, []string{h.Name, h.Title, strings.Join(transports, ","), user, project, h.Checked})
	}
	// CHECKED is when that harness's config shape was last confirmed against
	// the vendor. Nothing here can verify another product's file format, so
	// the date is what lets a reader judge how much to trust the row, and
	// `--json` carries the source URL beside it.
	return env.W.Table([]string{"NAME", "HARNESS", "TRANSPORTS", "USER FILE", "PROJECT FILE", "CHECKED"}, rows)
}

// serverEntry is the harness's entry with $URL, $KEY and $KEYENV filled in.
func serverEntry(env *Env, h agents.Harness, stdio bool) (map[string]any, error) {
	if stdio {
		if h.Stdio == nil {
			return nil, out.Failf("drop --stdio", "%s cannot run a local server; use the hosted form", h.Title)
		}
		return cloneMap(h.Stdio), nil
	}
	if h.HTTP == nil {
		return nil, out.Failf("pilot mcp install "+h.Name+" --stdio", "%s cannot dial a URL; register `pilot mcp` on stdio instead", h.Title)
	}
	// Only a harness that EMBEDS the token needs one on hand. Codex is handed
	// the variable's name and reads it itself, so requiring a stored key
	// there would refuse a setup that works, and would be asking for a secret
	// in order to not write it down.
	if h.NeedsKeyValue() && env.APIKey.Value == "" {
		return nil, out.Failf("run pilot login, or set "+agents.KeyEnvVar, "no API key to register for %s", env.APIURL.Value)
	}
	url := strings.TrimRight(env.APIURL.Value, "/") + "/mcp"
	return substitute(cloneMap(h.HTTP), url, env.APIKey.Value).(map[string]any), nil
}

func cloneMap(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func substitute(v any, url, key string) any {
	switch x := v.(type) {
	case string:
		// $KEYENV FIRST: it starts with $KEY, so replacing the short one
		// first turns "$KEYENV" into "<the token>ENV" and writes a secret
		// into a field that wanted a variable name.
		return strings.NewReplacer("$URL", url, "$KEYENV", agents.KeyEnvVar, "$KEY", key).Replace(x)
	case map[string]any:
		for k, val := range x {
			x[k] = substitute(val, url, key)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = substitute(val, url, key)
		}
		return x
	}
	return v
}

// harnessConfigPath picks the file for the scope: the repository one under
// the working directory, or the user one under HOME.
func harnessConfigPath(getenv config.Env, h agents.Harness, project bool) (string, error) {
	if project {
		if h.Project == "" {
			return "", out.Failf("drop --project", "%s has no per-repository config file", h.Title)
		}
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, filepath.FromSlash(h.Project)), nil
	}
	user := h.User
	if runtime.GOOS == "darwin" && h.UserDarwin != "" {
		user = h.UserDarwin
	}
	if user == "" {
		if h.Project == "" {
			return "", out.Failf("pilot mcp install --list", "%s has no config file", h.Title)
		}
		// generic: an mcp.json where you stand.
		dir, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, filepath.FromSlash(h.Project)), nil
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(user, "~/"))), nil
}

// writeEntry merges the entry into the file at path, creating it if needed.
// An existing pilots entry is REPLACED, because running the command again is
// how a user switches transports or rotates a key; anything else in the file
// is left exactly as it was.
func writeEntry(path string, h agents.Harness, entry map[string]any) (bool, error) {
	switch h.Format {
	case "toml":
		return writeTOMLEntry(path, h.Path, entry)
	default:
		return writeJSONEntry(path, h.Path, entry)
	}
}

func writeJSONEntry(path string, keyPath []string, entry map[string]any) (bool, error) {
	cfg := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return false, out.Failf("fix it or move it aside, then run the install again", "%s is not valid JSON", path)
		}
	}
	servers, _ := cfg[keyPath[0]].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if existing, ok := servers[keyPath[1]]; ok && sameJSON(existing, entry) {
		return false, nil
	}
	servers[keyPath[1]] = entry
	cfg[keyPath[0]] = servers
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	// 0600: the hosted entry carries the key.
	return true, os.WriteFile(path, append(raw, '\n'), 0o600)
}

func sameJSON(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

// writeTOMLEntry replaces or appends the `[mcp_servers.pilots]` table in a
// TOML file. A real TOML round-trip is not worth a dependency: the file is
// scanned for the one header this command owns, its lines up to the next
// header are replaced, and every other byte is kept.
func writeTOMLEntry(path string, keyPath []string, entry map[string]any) (bool, error) {
	header := "[" + strings.Join(keyPath, ".") + "]"
	section := renderTOML(header, entry)
	existing := ""
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	}
	lines := strings.Split(existing, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	var next string
	if start >= 0 {
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "[") {
				end = i
				break
			}
		}
		current := strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n")
		if current == strings.TrimRight(section, "\n") {
			return false, nil
		}
		next = strings.Join(lines[:start], "\n") + "\n" + section
		if end < len(lines) {
			next += "\n" + strings.Join(lines[end:], "\n")
		}
	} else {
		next = existing
		switch {
		case next == "":
		case strings.HasSuffix(next, "\n\n"):
		case strings.HasSuffix(next, "\n"):
			next += "\n"
		default:
			next += "\n\n"
		}
		next += section
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(strings.TrimRight(next, "\n")+"\n"), 0o600)
}

// renderTOML writes one table whose values are strings, string arrays, bools
// or one-level string maps, which is every shape the harness table uses.
func renderTOML(header string, entry map[string]any) string {
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, k := range keys {
		b.WriteString(k + " = " + tomlValue(entry[k]) + "\n")
	}
	return b.String()
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case bool:
		return fmt.Sprint(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = tomlValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + " = " + tomlValue(x[k])
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return fmt.Sprintf("%q", fmt.Sprint(v))
}

// renderEntry is what --print shows: the entry inside its key path, in the
// file's own format, so it can be pasted.
func renderEntry(h agents.Harness, entry map[string]any) (string, error) {
	if h.Format == "toml" {
		return strings.TrimRight(renderTOML("["+strings.Join(h.Path, ".")+"]", entry), "\n"), nil
	}
	doc := map[string]any{h.Path[0]: map[string]any{h.Path[1]: entry}}
	raw, err := json.MarshalIndent(doc, "", "  ")
	return string(raw), err
}

// checkHarnesses is the doctor's line: which coding agents on this machine
// have the server registered, read from the same table.
func checkHarnesses(getenv config.Env) check {
	var found []string
	for _, h := range agents.Harnesses() {
		if h.User == "" {
			continue
		}
		path, err := harnessConfigPath(getenv, h, false)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if h.Format == "toml" {
			if strings.Contains(string(raw), "["+strings.Join(h.Path, ".")+"]") {
				found = append(found, h.Name)
			}
			continue
		}
		var cfg map[string]any
		if json.Unmarshal(raw, &cfg) != nil {
			continue
		}
		if servers, _ := cfg[h.Path[0]].(map[string]any); servers != nil {
			if _, ok := servers[h.Path[1]]; ok {
				found = append(found, h.Name)
			}
		}
	}
	if len(found) == 0 {
		return check{Name: "agents", OK: true, Detail: "no coding agent has the MCP server registered",
			Fix: "pilot mcp install <harness>; pilot mcp install --list names them"}
	}
	return check{Name: "agents", OK: true, Detail: "MCP server registered in " + strings.Join(found, ", ")}
}
