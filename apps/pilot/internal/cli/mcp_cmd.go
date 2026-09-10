package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vivek7405/pilots/agents"
	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

func newMCPCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "mcp",
		Short: "run the pilots MCP server on stdio (for Claude Code, Claude Desktop, …)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			// Every diagnostic to stderr: stdout is the protocol channel.
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			return runMCP(c.Context(), mcpDeps{client: client, getenv: getenv, env: env, pages: loadSkill(skillRoot(getenv))})
		},
	}
	Describe(c, Doc{
		What: "The fleet as MCP tools: create_machine, exec, deploy, promote, logs,\n" +
			"checkpoint, diagnose and the rest, plus the pilots skill as resources\n" +
			"under pilots-docs:// and a `deploy` prompt. Every result carries\n" +
			"`next`; every error carries `code`, `next` and `details`.",
		How: "`pilot init` registers it in a repository's .mcp.json (Claude Code)\n" +
			"and .cursor/mcp.json as {\"type\":\"stdio\",\"command\":\"pilot\",\"args\":[\"mcp\"]}.\n" +
			"`pilot mcp install <harness>` writes the hosted form, https://<api>/mcp\n" +
			"with the key, for Codex, OpenCode, Cursor and the rest.\n" +
			"The key, fleet and org come from the same places every other command\n" +
			"uses, so `pilot login` once is enough for the agent too.",
		Examples: []string{
			"pilot init            # register it in this repository",
			"pilot mcp             # what the agent runs; not meant for a terminal",
		},
		Related: []string{
			"pilot mcp install     register the server with Claude Code, Codex, Cursor, …",
			"pilot init            copy the skill in and register the server",
			"pilot skill install   link the skill into ~/.claude/skills",
		},
	})
	c.AddCommand(newMCPInstallCmd(env, getenv))
	return c
}

var mcpEntry = map[string]any{"type": "stdio", "command": "pilot", "args": []string{"mcp"}}

func newInitCmd(env *Env, getenv config.Env) *cobra.Command {
	c := &cobra.Command{
		Use:   "init [dir]",
		Short: "copy the pilots skill into a repository and register the MCP server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			dir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			// The embedded pages when no copy is on disk, so an installed
			// binary never needs a checkout.
			pages := loadSkill(skillRoot(getenv))
			var done, skipped []string
			record := func(what string, changed bool) {
				if changed {
					done = append(done, what)
				} else {
					skipped = append(skipped, what)
				}
			}
			changed, err := copySkill(pages, filepath.Join(dir, ".agents", "skills", "pilots"))
			if err != nil {
				return err
			}
			record(".agents/skills/pilots", changed)
			for _, rel := range []string{".mcp.json", ".cursor/mcp.json"} {
				changed, err := mergeMCPConfig(filepath.Join(dir, filepath.FromSlash(rel)))
				if err != nil {
					return err
				}
				record(rel, changed)
			}
			changed, err = appendStanza(filepath.Join(dir, "AGENTS.md"))
			if err != nil {
				return err
			}
			record("AGENTS.md", changed)
			if env.W.JSON {
				return env.W.JSONValue(map[string]any{"dir": dir, "done": done, "skipped": skipped})
			}
			for _, d := range done {
				env.W.Notef("wrote %s", d)
			}
			for _, s := range skipped {
				env.W.Notef("kept %s (already set up)", s)
			}
			return nil
		},
	}
	Describe(c, Doc{
		What: "Everything an agent needs to deploy this repository: the skill pages\n" +
			"under .agents/skills/pilots, the MCP server registered for Claude Code\n" +
			"and Cursor, and a short stanza in AGENTS.md saying which call to make.",
		How: "Idempotent. A page directory that exists is left alone, a server\n" +
			"already registered is not registered twice, and the stanza is added\n" +
			"once. Run it again after `pilot skill install` upgrades the pages.",
		Examples: []string{"pilot init", "pilot init ~/src/shop"},
	})
	return c
}

func copySkill(pages []agents.Page, target string) (bool, error) {
	if hasSkill(target) {
		return false, nil
	}
	return true, writeSkill(pages, target)
}

func mergeMCPConfig(path string) (bool, error) {
	cfg := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return false, out.Failf("fix it or move it aside, then run pilot init again", "%s is not valid JSON", path)
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["pilots"]; ok {
		return false, nil
	}
	servers["pilots"] = mcpEntry
	cfg["mcpServers"] = servers
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(raw, '\n'), 0o644)
}

func appendStanza(path string) (bool, error) {
	existing := ""
	if raw, err := os.ReadFile(path); err == nil {
		existing = string(raw)
	}
	if strings.Contains(existing, agentsHeading) {
		return false, nil
	}
	sep := ""
	switch {
	case existing == "":
	case strings.HasSuffix(existing, "\n\n"):
	case strings.HasSuffix(existing, "\n"):
		sep = "\n"
	default:
		sep = "\n\n"
	}
	return true, os.WriteFile(path, []byte(existing+sep+agentsStanza), 0o644)
}

func newSkillCmd(env *Env, getenv config.Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "skill",
		Short: "manage the pilots agent skill",
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "link the pilots skill into ~/.claude/skills and keep a copy for `pilot init`",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			home := getenv("HOME")
			if home == "" {
				home, _ = os.UserHomeDir()
			}
			// The embedded pages, never a repository's edited copy: this is
			// the global install, and one team's edits must not become
			// everyone's. Rewritten every time, so an upgraded binary
			// upgrades the skill.
			share := filepath.Join(home, ".local", "share", "pilots", "skill")
			_ = os.RemoveAll(share)
			if err := writeSkill(agents.Pages(), share); err != nil {
				return err
			}
			target := filepath.Join(home, ".claude", "skills", "pilots")
			if st, err := os.Lstat(target); err == nil {
				if st.Mode()&os.ModeSymlink == 0 {
					return out.Failf("remove it first if you want the packaged skill", "%s is a directory, not a link", target)
				}
				_ = os.Remove(target)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(share, target); err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(map[string]string{"linked": target, "to": share})
			}
			env.W.Notef("linked %s -> %s", target, share)
			return nil
		},
	}
	Describe(install, Doc{
		How: "Writes the pages this binary embeds to ~/.local/share/pilots/skill\n" +
			"and links ~/.claude/skills/pilots at that copy so Claude Code loads\n" +
			"them everywhere. The Claude Code plugin is the other way to get\n" +
			"them: /plugin marketplace add vivek7405/pilots.",
		Examples: []string{"pilot skill install"},
	})
	root.AddCommand(install)
	return root
}

var _ = fmt.Sprintf
