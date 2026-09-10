package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/cli/internal/config"
)

// The skill: the same pages are three things at once -- files an agent reads
// off disk after `pilot init`, MCP resources under pilots-docs://, and the
// `docs` tool's answers. One copy on disk serves all three, so a fix to a
// page cannot land in one surface and miss the other two.
//
// Resolution order, first hit wins: a repository's own .agents/skills/pilots
// (walking up from the working directory), PILOT_SKILL_DIR, the checkout
// this binary was built in (packages/cli/skill/pilots beside apps/pilot),
// and finally ~/.local/share/pilots/skill, which `pilot skill install`
// populates so an installed binary has the pages without a checkout.
type skillPage struct {
	Name string // SKILL.md, or references/deploy.md
	Path string
}

func skillRoot(getenv config.Env) string {
	if dir, err := os.Getwd(); err == nil {
		for {
			if hasSkill(filepath.Join(dir, ".agents", "skills", "pilots")) {
				return filepath.Join(dir, ".agents", "skills", "pilots")
			}
			// Working inside the pilots checkout itself, wherever the binary
			// was built.
			if hasSkill(filepath.Join(dir, "packages", "cli", "skill", "pilots")) {
				return filepath.Join(dir, "packages", "cli", "skill", "pilots")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if d := getenv("PILOT_SKILL_DIR"); d != "" && hasSkill(d) {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			// A binary built anywhere inside a checkout finds the checkout's
			// pages by walking up to the repository root.
			for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
				if d := filepath.Join(dir, "packages", "cli", "skill", "pilots"); hasSkill(d) {
					return d
				}
				if filepath.Dir(dir) == dir {
					break
				}
			}
		}
	}
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if d := filepath.Join(home, ".local", "share", "pilots", "skill"); hasSkill(d) {
		return d
	}
	return ""
}

func hasSkill(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !st.IsDir()
}

// skillPages lists SKILL.md first, then the references, sorted.
func skillPages(root string) []skillPage {
	if root == "" {
		return nil
	}
	pages := []skillPage{{Name: "SKILL.md", Path: filepath.Join(root, "SKILL.md")}}
	refs := filepath.Join(root, "references")
	entries, err := os.ReadDir(refs)
	if err != nil {
		return pages
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		pages = append(pages, skillPage{Name: "references/" + n, Path: filepath.Join(refs, n)})
	}
	return pages
}

// topics is what `docs` accepts, derived from the pages rather than listed
// twice.
func topics(root string) []string {
	// Never nil: an agent reading `topics` gets a list, not JSON null.
	out := []string{}
	for _, p := range skillPages(root) {
		if strings.HasPrefix(p.Name, "references/") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(p.Name, "references/"), ".md"))
		}
	}
	return out
}

func readTopic(root, topic string) (string, bool) {
	for _, p := range skillPages(root) {
		if p.Name == "references/"+topic+".md" {
			raw, err := os.ReadFile(p.Path)
			if err != nil {
				return "", false
			}
			return string(raw), true
		}
	}
	return "", false
}

type topicMatch struct {
	Topic   string `json:"topic"`
	Excerpt string `json:"excerpt"`
}

// searchTopics is a case-insensitive substring search with one excerpt per
// page: enough to pick a page, not a search engine.
func searchTopics(root, query string) []topicMatch {
	q := strings.ToLower(query)
	var out []topicMatch
	for _, p := range skillPages(root) {
		if !strings.HasPrefix(p.Name, "references/") {
			continue
		}
		raw, err := os.ReadFile(p.Path)
		if err != nil {
			continue
		}
		text := string(raw)
		i := strings.Index(strings.ToLower(text), q)
		if i < 0 {
			continue
		}
		start, end := max(0, i-80), min(len(text), i+len(q)+80)
		out = append(out, topicMatch{
			Topic:   strings.TrimSuffix(strings.TrimPrefix(p.Name, "references/"), ".md"),
			Excerpt: strings.TrimSpace(text[start:end]),
		})
	}
	return out
}

// primer is the `init` tool's answer. Under sixty lines on purpose: it is the
// first thing a small model reads, it competes for the same context as the
// task, and a primer nobody finishes is worse than none. A test holds it to
// the budget, because the natural drift is upward.
const primer = `pilots: sandboxes and services on one primitive.

THE ONE CALL
  deploy { "dir": "<absolute path>" }
  -> { app, services: [{ name, url, release_id }], next }
  The host decides what the directory is: a compose file, a Dockerfile,
  a recipe (webjs, next, react-router, vite, django, fastapi, rails, go,
  rust, laravel), or unknown. Do not write a Dockerfile first.

EVERY RESULT CARRIES next. EVERY ERROR CARRIES code, next, details.
  Read next. Do that. Nothing else needs planning.

THE ANSWERS YOU WILL SEE
  unknown_framework   read details.listing and details.manifests, write a
                      Dockerfile that obeys details.rules, call build with
                      it, then deploy with name and build.
  build_failed        every log line is in the error; fix the line marked
                      error, call build again.
  health_gate_failed  call diagnose with details.replica; it is almost
                      always the port (read $PORT, 8080) or the bind
                      address (0.0.0.0, never 127.0.0.1).
  plan_unsupported    fix each key in details.unsupported.
  plan_multi_service  commit a compose file; a push deploys one service.
  quota_exceeded      next names the limit.
  not_found           check the id; the key may see a different org.

THE PRIMITIVE
  A machine is a Firecracker microVM. A sandbox and a production replica
  are the same machine with different lifecycle knobs. A service is one
  or more machines behind a permanent URL that survives every deploy.
  create_machine + exec is a sandbox. deploy is a service. promote turns
  the first into the second without changing its URL. A quiet machine
  suspends (a freeze; it resumes on the next exec or request); a console
  running a command keeps it up, idle_timeout sets the wait for a daemon.
  A cron is a GET on a path on a schedule (schedules on create or deploy;
  webjs.crons in package.json); the host wakes the machine for it.

RULES
  No directory and no repo in the conversation: ask, never invent one.
  destroy_machine and rollback change what is live: confirm first.
  After a mutation, read it back with service or status.
  Secrets are secret:// references in a compose file; never paste values.

DOCS
  docs { "topic": "deploy" | "sandboxes" | "services" | "secrets" |
         "volumes" | "domains" | "promote" | "errors" | "compose" }
  Load one. Two at most.
`

// The stanza `pilot init` appends to AGENTS.md, matched by its first line
// on a re-run.
const agentsHeading = "## Deploying with pilots"

const agentsStanza = agentsHeading + `

This repository deploys to pilots. The skill is at ` + "`.agents/skills/pilots/`" + `;
read ` + "`SKILL.md`" + ` before deploying, and load at most two reference pages.

The one call is ` + "`pilot deploy`" + ` in the app's directory, or the MCP ` + "`deploy`" + `
tool with ` + "`dir`" + `. The platform decides what the directory is. Do not write a
Dockerfile or a compose file first; write one only when the answer is
` + "`unknown_framework`" + `, and obey the two rules its ` + "`details.rules`" + ` names.

Every error carries ` + "`code`" + `, ` + "`next`" + ` and ` + "`details`" + `. Read ` + "`next`" + ` and do that.
`
