package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Remix 3 is the bare `remix` package, and it is a different framework from
// the lineage `@remix-run/*` names -- that one became react-router. Remix 3
// ships no remix.config.*, no react-router.config.* and no vite.config.*, so
// before isRemix3 existed a Remix 3 app matched nothing and fell through to
// unknown: `pilot deploy` answered with the write-a-Dockerfile loop for an app
// the platform could have deployed on its own.
//
// The fixtures are the real shapes, copied from the demos in the remix-the-web
// repository: `remix` in dependencies, engines.node >= 24.3.0, a server.ts,
// and a start script of `node --import remix/node-tsx server.ts`.
func TestRemix3IsDetectedWithoutAnyConfigFile(t *testing.T) {
	dir := filepath.Join(fixtures, "remix")
	if got := Detect(dir); got != FrameworkRemix {
		t.Fatalf("Detect = %q, want %q", got, FrameworkRemix)
	}
	for _, f := range []string{"remix.config.js", "react-router.config.ts", "vite.config.ts", "next.config.js"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("the fixture has %s, so it proves nothing about a Remix 3 app, which has none", f)
		}
	}
}

// The two lineages share a word and nothing else. Whichever way a directory
// leans, it must not land on the other's recipe: react-router's runs
// `npm run build`, and a Remix 3 app has no build script, so the image would
// fail on a script that does not exist.
func TestRemix3IsNotConfusedWithTheReactRouterLineage(t *testing.T) {
	// A Remix 1 app: it also depended on a package called `remix`, but it
	// carried remix.config.js, which the switch checks first.
	v1 := t.TempDir()
	write(t, v1, "package.json", `{"name":"old","dependencies":{"remix":"1.19.3"}}`)
	write(t, v1, "remix.config.js", "module.exports = {}\n")
	if got := Detect(v1); got != FrameworkReactRouter {
		t.Errorf("a remix.config.js project = %q, want %q", got, FrameworkReactRouter)
	}

	// A Remix v2 app with no config file: the @remix-run/* scope is what
	// names it, and it is react-router's, not Remix 3's.
	v2 := t.TempDir()
	write(t, v2, "package.json", `{"name":"v2","dependencies":{"@remix-run/node":"2.15.0","@remix-run/react":"2.15.0"}}`)
	if got := Detect(v2); got == FrameworkRemix {
		t.Error("a @remix-run/* project was claimed by the Remix 3 recipe")
	}

	// Both, which is a v2 app mid-migration. The scope wins.
	both := t.TempDir()
	write(t, both, "package.json", `{"name":"both","dependencies":{"remix":"3.0.0","@remix-run/react":"2.15.0"}}`)
	if got := Detect(both); got == FrameworkRemix {
		t.Error("a project carrying @remix-run/* was claimed by the Remix 3 recipe")
	}

	// A package that merely mentions remix in another dependency's name is
	// not a Remix app.
	near := t.TempDir()
	write(t, near, "package.json", `{"name":"near","dependencies":{"remix-auth":"3.7.0","eslint-plugin-remix":"1.0.0"}}`)
	if got := Detect(near); got == FrameworkRemix {
		t.Error("remix-auth alone was read as Remix 3")
	}
}

// Buildless is the whole point: `npm start` runs the TypeScript through
// remix/node-tsx. A `npm run build` in this recipe fails the image.
func TestRemix3RecipeIsBuildless(t *testing.T) {
	r, ok := Generate(filepath.Join(fixtures, "remix"))
	if !ok {
		t.Fatal("Generate refused a Remix 3 directory")
	}
	if r.Framework != FrameworkRemix {
		t.Fatalf("framework = %q", r.Framework)
	}
	if strings.Contains(r.Dockerfile, "npm run build") {
		t.Errorf("the recipe runs a build a Remix 3 app does not have:\n%s", r.Dockerfile)
	}
	if !strings.Contains(r.Dockerfile, "FROM node:24-alpine") {
		t.Errorf("remix requires node >= 24.3.0:\n%s", r.Dockerfile)
	}
	if !strings.Contains(r.Dockerfile, "npm start") {
		t.Errorf("the recipe never starts the app:\n%s", r.Dockerfile)
	}
	if r.Port != AppPort || r.Health == nil || r.Health.Path != "/" {
		t.Errorf("port = %d health = %+v", r.Port, r.Health)
	}
	// The migration must NOT be there: this fixture declares no remix.json,
	// and `remix db migrate` against a project with no database configured
	// fails the start.
	if strings.Contains(r.Dockerfile, "db migrate") {
		t.Errorf("a migration ran for a project that declares none:\n%s", r.Dockerfile)
	}
}

// remix.json is where Remix 3 declares its database. A project that declares
// migrations gets them, and is told that a sqlite file needs a volume -- the
// failure otherwise is silent and only visible one release later, when the
// data is gone.
func TestRemix3RunsDeclaredMigrations(t *testing.T) {
	r, ok := Generate(filepath.Join(fixtures, "remix-db"))
	if !ok {
		t.Fatal("Generate refused the Remix 3 database fixture")
	}
	if !strings.Contains(r.Dockerfile, "remix db migrate && npm start") {
		t.Errorf("remix.json declares migrations and none run:\n%s", r.Dockerfile)
	}
	notes := strings.Join(r.Notes, "\n")
	if !strings.Contains(notes, "volume") {
		t.Errorf("a sqlite adapter with no volume note:\n%s", notes)
	}
	if !strings.Contains(notes, "pre_deploy") {
		t.Errorf("no note about replicas running the migration more than once:\n%s", notes)
	}

	// A remix.json with a database but no migrations directory declares no
	// migrations, and inventing one would fail the start.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"x","dependencies":{"remix":"3.0.0"}}`)
	write(t, dir, "remix.json", `{"db":{"adapter":{"type":"sqlite","filename":"./x.sqlite"}}}`)
	r2, _ := Generate(dir)
	if strings.Contains(r2.Dockerfile, "db migrate") {
		t.Errorf("a migration ran for a remix.json with no migrations directory:\n%s", r2.Dockerfile)
	}

	// Unparseable remix.json answers "no migrations" rather than erroring:
	// the recipe still works, it just does not run a migration it cannot
	// confirm was asked for.
	broken := t.TempDir()
	write(t, broken, "package.json", `{"name":"x","dependencies":{"remix":"3.0.0"}}`)
	write(t, broken, "remix.json", `{ not json`)
	if got := Detect(broken); got != FrameworkRemix {
		t.Fatalf("a broken remix.json changed detection: %q", got)
	}
	r3, ok := Generate(broken)
	if !ok || strings.Contains(r3.Dockerfile, "db migrate") {
		t.Errorf("a broken remix.json produced ok=%v and:\n%s", ok, r3.Dockerfile)
	}
}
