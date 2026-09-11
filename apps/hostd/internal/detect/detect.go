// Package detect decides what a directory is, so nobody has to guess.
//
// It answers one question: given a tree of files, what does pilots run, on
// what port, with what health check. The answer is deterministic code and
// never a model, because a model asked "what framework is this" is wrong
// occasionally and expensively: the build succeeds, the URL answers 502, and
// the cost lands on whoever is reading the 502.
//
// It lives on the host rather than in the CLI, and that is the point of the
// package. The CLI, the MCP server, the dashboard and the GitHub push path all
// need the same answer; a copy in the CLI was invisible to three of them and
// could drift from the fourth with nothing going red.
//
// Two mistakes produce a build that SUCCEEDS and a URL that answers 502, so
// every recipe here avoids both and Rules names them for anyone writing a
// Dockerfile by hand:
//
//  1. binding 127.0.0.1 instead of 0.0.0.0, so the guest serves only itself
//     and the router's proxy into the netns reaches nothing;
//  2. ignoring $PORT, so the app listens somewhere the router is not looking.
//
// A 127.0.0.1 is right in exactly one place, a HEALTHCHECK line: that probe
// runs inside the guest, where the loopback is the correct target.
package detect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// Framework is what a directory was recognised as.
type Framework string

const (
	FrameworkWebJS       Framework = "webjs"
	FrameworkNext        Framework = "next"
	FrameworkReactRouter Framework = "react-router"
	FrameworkRemix       Framework = "remix"
	FrameworkVite        Framework = "vite"
	FrameworkDjango      Framework = "django"
	FrameworkFastAPI     Framework = "fastapi"
	FrameworkRails       Framework = "rails"
	FrameworkGo          Framework = "go"
	FrameworkRust        Framework = "rust"
	FrameworkLaravel     Framework = "laravel"
	FrameworkUnknown     Framework = "unknown"
)

// AppPort is the port every recipe declares and the router dials.
//
// The framework's own default is not used, and that is deliberate: a recipe
// that said 3000 for Next and 8000 for Django left the platform's port and the
// image's port disagreeing, which is the 502 above with extra steps.
const AppPort = 8080

// LookedFor is every signal the detector checks, in order. It travels on an
// unknown answer so the refusal says what was actually looked at rather than
// only that nothing matched.
var LookedFor = []string{
	"package.json (with a @webjsdev/* dependency)",
	"next.config.{js,ts,mjs,cjs}",
	"react-router.config.* or remix.config.*",
	"package.json with a bare `remix` dependency (Remix 3)",
	"vite.config.*",
	"manage.py with requirements.txt or pyproject.toml",
	"main.py or app.py importing fastapi",
	"Gemfile with bin/rails",
	"go.mod",
	"Cargo.toml",
	"composer.json with artisan",
}

// Rules are the two lines every Dockerfile must obey. They are on every
// unknown answer and in every tool description, because the model that has to
// obey them may have loaded no documentation at all.
var Rules = []string{
	"bind 0.0.0.0, never 127.0.0.1: a service bound to loopback serves only the guest and the router reaches nothing",
	"read the port from $PORT with 8080 as the fallback; the router dials 8080",
}

var lockfiles = []string{
	"package-lock.json", "npm-shrinkwrap.json", "yarn.lock",
	"pnpm-lock.yaml", "bun.lockb",
}

// Detect names the framework a directory holds, in LookedFor's order.
//
// webjs is first and is detected by dependency rather than by a config file,
// because a webjs app has no build step and no bundler config to find. That is
// the framework's whole point and it is also why nothing else can stand in for
// the check.
func Detect(dir string) Framework { return DetectIn(dir, dir) }

// DetectIn is Detect for a directory whose lockfile lives somewhere else.
//
// npm hoists the lockfile to the workspace root, so a member of a monorepo has
// a package.json and a framework config and no lockfile of its own. Checking
// for one in the member is then a check that can never pass: a Next workspace
// fell straight through to unknown and was silently dropped, which took a
// monorepo of two Next apps to zero services and an unknown_framework answer.
//
// lockRoot is the workspace root for a member, and the directory itself
// otherwise. It is only ever consulted for lockfiles: everything else about a
// framework is declared in the member.
func DetectIn(dir, lockRoot string) Framework {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	hasPrefix := func(prefix string) bool {
		for _, name := range listing(dir) {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	anyLockfile := func() bool {
		for _, l := range lockfiles {
			if _, err := os.Stat(filepath.Join(lockRoot, l)); err == nil {
				return true
			}
		}
		return false
	}

	if pkg := readPackageJSON(dir); pkg != nil {
		for _, name := range dependencyNames(pkg) {
			if strings.HasPrefix(name, "@webjsdev/") {
				return FrameworkWebJS
			}
		}
	}
	switch {
	case hasPrefix("next.config.") && anyLockfile():
		return FrameworkNext
	case hasPrefix("react-router.config.") || hasPrefix("remix.config."):
		return FrameworkReactRouter
	case isRemix3(dir):
		return FrameworkRemix
	case hasPrefix("vite.config."):
		return FrameworkVite
	case has("manage.py") && (has("requirements.txt") || has("pyproject.toml")):
		return FrameworkDjango
	case importsFastAPI(dir):
		return FrameworkFastAPI
	case has("Gemfile") && has(filepath.Join("bin", "rails")):
		return FrameworkRails
	case has("go.mod"):
		return FrameworkGo
	case has("Cargo.toml"):
		return FrameworkRust
	case has("composer.json") && has("artisan"):
		return FrameworkLaravel
	}
	return FrameworkUnknown
}

// ---------------------------------------------------------------------------
// Detection helpers. Every one answers "not there" rather than erroring: a
// directory the detector cannot read is a directory the next signal decides.
// ---------------------------------------------------------------------------

func listing(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// declaredCrons reads the cron jobs an app declares in ITS OWN config, so a
// cron needs no pilots-specific file for anybody:
//
//	vercel.json     { "crons": [{ "path": "/api/digest", "schedule": "0 5 * * *" }] }
//	package.json    "webjs": { "crons": [ … same shape … ] }
//
// vercel.json is what Next, Astro, SvelteKit, Nuxt and Remix users already
// write, and it is read for EVERY app rather than for a list of frameworks:
// the file is an explicit declaration, and which framework wrote it is not
// this package's business. webjs spells the same thing in the block it
// already owns, with Vercel's field names, and wins where both are present --
// the framework's own file is the more specific of the two.
//
// The result is nil when nothing declares any, and an empty, non-nil list
// when a config declares an empty one: the deploy merges knobs onto the
// previous release's, and only an explicit empty list clears inherited crons.
//
// A malformed entry is an error naming its file and index, not a warning: a
// cron silently dropped at plan time fires nowhere and nobody is told.
func declaredCrons(dir string, fw Framework) ([]api.Schedule, error) {
	if fw == FrameworkWebJS {
		// nil AND no error means "this file declares nothing", which is the
		// only case that falls through to the other one.
		if crons, err := webjsCrons(dir); crons != nil || err != nil {
			return crons, err
		}
	}
	return vercelCrons(dir)
}

func webjsCrons(dir string) ([]api.Schedule, error) {
	pkg := readPackageJSON(dir)
	block, _ := pkg["webjs"].(map[string]any)
	raw, ok := block["crons"]
	if !ok {
		return nil, nil
	}
	return cronEntries("package.json: webjs.crons", raw)
}

// vercelCrons reads vercel.json's crons.
//
// A file that does not parse declares nothing readable and is ignored, the
// same answer readPackageJSON gives: refusing the deploy would make an
// unrelated broken file -- one this platform has no other use for -- the
// reason an app cannot ship. A file that DOES parse and spells crons wrongly
// is an error, because there the author was talking to us.
func vercelCrons(dir string) ([]api.Schedule, error) {
	body, err := os.ReadFile(filepath.Join(dir, "vercel.json"))
	if err != nil {
		return nil, nil
	}
	var cfg map[string]any
	if json.Unmarshal(body, &cfg) != nil {
		return nil, nil
	}
	raw, ok := cfg["crons"]
	if !ok {
		return nil, nil
	}
	return cronEntries("vercel.json: crons", raw)
}

// cronEntries turns Vercel's [{path, schedule}] into schedules. One parser for
// both files, because webjs copied those field names deliberately and two
// copies would drift on the day one of them gained a field.
func cronEntries(source string, raw any) ([]api.Schedule, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of {path, schedule}", source)
	}
	if len(list) > api.MaxSchedules {
		return nil, fmt.Errorf("%s has %d entries, more than the %d a machine may carry",
			source, len(list), api.MaxSchedules)
	}
	out := make([]api.Schedule, 0, len(list))
	for i, item := range list {
		entry, _ := item.(map[string]any)
		path, _ := entry["path"].(string)
		schedule, _ := entry["schedule"].(string)
		s := api.Schedule{Cron: schedule, Path: path}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("%s[%d] %v", source, i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func readPackageJSON(dir string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg map[string]any
	if json.Unmarshal(raw, &pkg) != nil {
		return nil
	}
	return pkg
}

func dependencyNames(pkg map[string]any) []string {
	var out []string
	for _, field := range []string{"dependencies", "devDependencies", "peerDependencies"} {
		deps, ok := pkg[field].(map[string]any)
		if !ok {
			continue
		}
		for name := range deps {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

var fastAPIImport = regexp.MustCompile(`(?m)^\s*(from\s+fastapi|import\s+fastapi)`)

func importsFastAPI(dir string) bool {
	for _, name := range []string{"main.py", "app.py"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if fastAPIImport.Match(raw) {
			return true
		}
	}
	return false
}

// findWSGIProject is the package directory holding wsgi.py, which names the
// WSGI module a Django recipe starts.
func findWSGIProject(dir string) string {
	for _, name := range listing(dir) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name, "wsgi.py")); err == nil {
			return name
		}
	}
	return ""
}

var staticRoot = regexp.MustCompile(`(?m)^\s*STATIC_ROOT\s*=`)

// hasStaticRoot reports whether collectstatic would have anywhere to write.
//
// A bare django-admin startproject sets no STATIC_ROOT, and collectstatic
// without one fails the start with an ImproperlyConfigured, so the step is
// added only when the setting is there.
func hasStaticRoot(dir, project string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, project, "settings.py"))
	if err != nil {
		return false
	}
	return staticRoot.Match(raw)
}

var cargoName = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)

func cargoPackageName(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return ""
	}
	if m := cargoName.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

// isRemix3 answers whether a directory is a Remix 3 app.
//
// Remix 3 is the bare `remix` package. That is a different framework from the
// one `@remix-run/*` names: that lineage became react-router, and the
// react-router recipe above is its recipe. The two are told apart by the
// dependency rather than by a file, because Remix 3 ships NO configuration
// file the detector could key on -- no remix.config.*, no
// react-router.config.*, and no vite.config.* -- which is why a Remix 3 app
// fell all the way through to unknown before this existed.
//
// The remix.config.* case is checked before this one, so a Remix 1 app, which
// also depended on a package called `remix`, is claimed by react-router first
// and never reaches here. The @remix-run/* guard is the belt to that
// suspenders: a directory carrying both is v2, not v3.
func isRemix3(dir string) bool {
	pkg := readPackageJSON(dir)
	if pkg == nil {
		return false
	}
	found := false
	for _, name := range dependencyNames(pkg) {
		if strings.HasPrefix(name, "@remix-run/") {
			return false
		}
		if name == "remix" {
			found = true
		}
	}
	return found
}
