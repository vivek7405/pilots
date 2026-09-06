package detect

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fixtures = "testdata/frameworks"

type row struct {
	fixture   string
	framework Framework
	base      string
	health    string
}

// Every row's port is 8080. It used to be the framework's own default, 3000
// for Next and 8000 for Django, which meant the image listened where the
// router was not looking: a build that succeeded and a URL that answered 502.
var rows = []row{
	{"webjs", FrameworkWebJS, "FROM node:24-alpine", "/__webjs/ready"},
	{"next", FrameworkNext, "FROM node:24-alpine", "/"},
	{"react-router", FrameworkReactRouter, "FROM node:24-alpine", "/"},
	{"vite", FrameworkVite, "FROM nginx:alpine", "/"},
	{"django", FrameworkDjango, "FROM python:3.12-slim", "/"},
	{"fastapi", FrameworkFastAPI, "FROM python:3.12-slim", "/"},
	{"rails", FrameworkRails, "FROM ruby:3.3-slim", "/up"},
	{"go", FrameworkGo, "FROM gcr.io/distroless/static", "/"},
	{"rust", FrameworkRust, "FROM debian:bookworm-slim", "/"},
	{"laravel", FrameworkLaravel, "FROM php:8.3-cli", "/"},
}

func TestEachFixtureIsDetectedAndGetsItsOwnRecipe(t *testing.T) {
	for _, r := range rows {
		t.Run(r.fixture, func(t *testing.T) {
			dir := filepath.Join(fixtures, r.fixture)
			if got := Detect(dir); got != r.framework {
				t.Fatalf("Detect = %q, want %q", got, r.framework)
			}
			recipe, ok := Generate(dir)
			if !ok {
				t.Fatal("Generate refused a directory Detect recognised")
			}
			if recipe.Framework != r.framework {
				t.Errorf("framework = %q, want %q", recipe.Framework, r.framework)
			}
			if recipe.Port != AppPort {
				t.Errorf("port = %d, want %d", recipe.Port, AppPort)
			}
			if recipe.Health == nil || recipe.Health.Path != r.health {
				t.Errorf("health = %+v, want path %q", recipe.Health, r.health)
			}
			if !strings.Contains(recipe.Dockerfile, r.base) {
				t.Errorf("the recipe does not use %s", r.base)
			}
			if len(recipe.Notes) == 0 {
				t.Error("every recipe says something the agent needs to know")
			}
		})
	}
}

// Every recipe declares the platform's port. Without the ENV line the app
// falls back to whatever its framework defaults to, which is not the port the
// health check polls.
func TestEveryRecipeDeclaresPort8080(t *testing.T) {
	envPort := regexp.MustCompile(`(?:^|\s)PORT=(\d+)`)
	for _, r := range rows {
		recipe, _ := Generate(filepath.Join(fixtures, r.fixture))
		var declared string
		for _, line := range strings.Split(recipe.Dockerfile, "\n") {
			if !strings.HasPrefix(line, "ENV ") {
				continue
			}
			if m := envPort.FindStringSubmatch(line); m != nil {
				declared = m[1]
				break
			}
		}
		if declared != "8080" {
			t.Errorf("%s declares PORT=%q, want 8080", r.fixture, declared)
		}
		if !strings.Contains(recipe.Dockerfile, "EXPOSE 8080") {
			t.Errorf("%s does not EXPOSE 8080", r.fixture)
		}
	}
}

// A hard-coded port in the start command is a service listening where the
// router is not looking, and nothing in the build log says so.
func TestAStartCommandNamingAPortInterpolatesIt(t *testing.T) {
	digits := regexp.MustCompile(`\d{2,5}`)
	for _, r := range rows {
		recipe, _ := Generate(filepath.Join(fixtures, r.fixture))
		var start []string
		for _, line := range strings.Split(recipe.Dockerfile, "\n") {
			if strings.HasPrefix(line, "CMD ") {
				start = append(start, line)
			}
		}
		text := strings.Join(start, "\n")
		if !digits.MatchString(text) {
			continue
		}
		if !strings.Contains(text, "${PORT") {
			t.Errorf("%s hard-codes a port in its CMD: %s", r.fixture, text)
		}
	}
}

func TestEveryRecipeBindsAllInterfacesOrWarns(t *testing.T) {
	binds := regexp.MustCompile(`0\.0\.0\.0|HOST=0\.0\.0\.0|listen \$\{PORT\}`)
	// These three bind inside the application rather than in the Dockerfile,
	// so the recipe cannot enforce it; the notes have to say so instead.
	inApp := map[string]bool{"webjs": true, "go": true, "rust": true}

	for _, r := range rows {
		recipe, _ := Generate(filepath.Join(fixtures, r.fixture))
		if inApp[r.fixture] {
			warned := false
			for _, n := range recipe.Notes {
				if strings.Contains(n, "0.0.0.0") || strings.Contains(n, "all interfaces") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("%s neither binds nor warns about binding", r.fixture)
			}
			continue
		}
		if !binds.MatchString(recipe.Dockerfile) {
			t.Errorf("%s does not bind 0.0.0.0", r.fixture)
		}
	}
}

// The probe runs inside the guest, where loopback is right. Anywhere else it
// is a service that answers only itself.
func TestLoopbackAppearsOnlyOnAHealthcheckLine(t *testing.T) {
	for _, r := range rows {
		recipe, _ := Generate(filepath.Join(fixtures, r.fixture))
		for _, line := range strings.Split(recipe.Dockerfile, "\n") {
			if !strings.Contains(line, "127.0.0.1") {
				continue
			}
			if strings.Contains(line, "HEALTHCHECK") || strings.Contains(line, `CMD ["node", "-e"`) {
				continue
			}
			t.Errorf("%s binds loopback outside a health probe: %s", r.fixture, line)
		}
	}
}

func TestTheWebJSRecipeIsTheScaffoldDockerfile(t *testing.T) {
	recipe, _ := Generate(filepath.Join(fixtures, "webjs"))
	want := `FROM node:24-alpine
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY package.json package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY . .
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s --start-period=40s --retries=5 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT||8080)+'/__webjs/ready').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
CMD ["npm", "start"]
`
	if recipe.Dockerfile != want {
		t.Errorf("the webjs recipe drifted from the scaffold:\n%s", recipe.Dockerfile)
	}
	if recipe.Health.GraceSec != 40 {
		t.Errorf("grace = %d, want 40: the readiness gate answers 503 until warm",
			recipe.Health.GraceSec)
	}
}

func TestTheDjangoRecipeNamesTheWSGIProject(t *testing.T) {
	recipe, _ := Generate(filepath.Join(fixtures, "django"))
	if !strings.Contains(recipe.Dockerfile,
		"gunicorn mysite.wsgi:application --bind 0.0.0.0:${PORT:-8080}") {
		t.Errorf("the start command does not name mysite:\n%s", recipe.Dockerfile)
	}
	if !strings.Contains(recipe.Dockerfile, "python manage.py migrate --noinput") {
		t.Error("the recipe does not migrate at start")
	}
	warned := false
	for _, n := range recipe.Notes {
		if strings.Contains(n, "ALLOWED_HOSTS") && strings.Contains(n, "DisallowedHost") {
			warned = true
		}
	}
	if !warned {
		t.Error("the one thing a bare startproject gets wrong on any host is not in the notes")
	}
}

func TestCollectstaticIsAddedOnlyWhenStaticRootIsSet(t *testing.T) {
	bare, _ := Generate(filepath.Join(fixtures, "django"))
	if strings.Contains(bare.Dockerfile, "collectstatic") {
		t.Error("a bare project has no STATIC_ROOT to collect into, and the step fails the start")
	}
	withRoot, _ := Generate(filepath.Join(fixtures, "django-static"))
	if !strings.Contains(withRoot.Dockerfile, "collectstatic --noinput") {
		t.Error("STATIC_ROOT is set and collectstatic is missing")
	}
	if !strings.Contains(withRoot.Dockerfile, "gunicorn shopsite.wsgi:application") {
		t.Error("the start command does not name shopsite")
	}
}

func TestTheRustRecipeCopiesTheDeclaredBinary(t *testing.T) {
	recipe, _ := Generate(filepath.Join(fixtures, "rust"))
	if !strings.Contains(recipe.Dockerfile, "target/release/shopserver /app") {
		t.Errorf("the recipe does not copy the declared binary:\n%s", recipe.Dockerfile)
	}
}

// webjs is buildless and has no config file of its own, so it has to be
// detected first or a stray bundler config takes the app somewhere else.
func TestAWebJSAppWinsOverAnyOtherSignal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"@webjsdev/webjs":"1","vite":"6"}}`)
	write(t, dir, "vite.config.ts", "export default {}")
	if got := Detect(dir); got != FrameworkWebJS {
		t.Fatalf("Detect = %q, want webjs", got)
	}
}

func TestNextNeedsALockfileAsWellAsAConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "next.config.js", "module.exports = {}")
	write(t, dir, "package.json", "{}")
	if got := Detect(dir); got != FrameworkUnknown {
		t.Fatalf("Detect = %q, want unknown: npm ci without a lockfile fails the build", got)
	}
}

func TestAReadmeAloneIsNotAFramework(t *testing.T) {
	if got := Detect(filepath.Join(fixtures, "unknown")); got != FrameworkUnknown {
		t.Fatalf("Detect = %q, want unknown", got)
	}
}

// The resolution ladder, rung by rung. Each step removes the winner and
// asserts the next one takes over, which is the only way to prove the order
// rather than prove one case.
func TestTheResolutionOrderIsComposeThenDockerfileThenRecipe(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"fx","dependencies":{"@webjsdev/core":"1"}}`)
	write(t, dir, "Dockerfile", "FROM scratch\n")
	write(t, dir, "compose.yaml", "services:\n  api:\n    build: .\n")

	res, planErr, unknown, err := Plan(context.Background(), dir, Options{App: "fx"})
	mustPlan(t, res, planErr, unknown, err)
	if res.Detected[0].Source != "compose" {
		t.Fatalf("source = %q, want compose: a file the repo wrote beats one the platform would write",
			res.Detected[0].Source)
	}

	rm(t, dir, "compose.yaml")
	res, planErr, unknown, err = Plan(context.Background(), dir, Options{App: "fx"})
	mustPlan(t, res, planErr, unknown, err)
	if res.Detected[0].Source != "dockerfile" {
		t.Fatalf("source = %q, want dockerfile", res.Detected[0].Source)
	}
	if res.Plan.Steps[0].Dockerfile != "" {
		t.Error("a Dockerfile rung must not carry generated text: the repo's file is the build's")
	}

	rm(t, dir, "Dockerfile")
	res, planErr, unknown, err = Plan(context.Background(), dir, Options{App: "fx"})
	mustPlan(t, res, planErr, unknown, err)
	if res.Detected[0].Source != "recipe" || res.Detected[0].Framework != "webjs" {
		t.Fatalf("detected = %+v, want a webjs recipe", res.Detected[0])
	}
	if !strings.Contains(res.Plan.Steps[0].Dockerfile, "ENV PORT=8080") {
		t.Error("the recipe's text did not reach the step")
	}

	rm(t, dir, "package.json")
	_, _, unknown, err = Plan(context.Background(), dir, Options{App: "fx"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if unknown == nil {
		t.Fatal("an empty directory planned as something")
	}
	if len(unknown.Details.LookedFor) != 10 {
		t.Errorf("looked_for has %d entries, want 10", len(unknown.Details.LookedFor))
	}
	if len(unknown.Details.Rules) != 2 {
		t.Errorf("rules has %d entries, want 2", len(unknown.Details.Rules))
	}
}

func TestAnUnknownAnswerCarriesTheManifestsAnAgentNeeds(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "README.md", "# a project")
	write(t, dir, "Makefile", "all:\n\techo hi\n")
	write(t, dir, "requirements.txt", "flask\n")

	_, _, unknown, err := Plan(context.Background(), dir, Options{})
	if err != nil || unknown == nil {
		t.Fatalf("Plan = %v, %v; want an unknown answer", unknown, err)
	}
	if unknown.Details.Manifests["requirements.txt"] != "flask\n" {
		t.Errorf("manifests = %v, want the requirements file", unknown.Details.Manifests)
	}
	if len(unknown.Details.Listing) != 3 {
		t.Errorf("listing = %v, want all three entries", unknown.Details.Listing)
	}
}

// The monorepo case: one service per workspace, each built from the root.
func TestAWorkspaceRepoPlansOneServicePerMember(t *testing.T) {
	res, planErr, unknown, err := Plan(context.Background(), "testdata/workspace-app",
		Options{App: "shop"})
	mustPlan(t, res, planErr, unknown, err)

	if len(res.Plan.Steps) != 2 {
		t.Fatalf("%d steps, want 2: %+v", len(res.Plan.Steps), res.Plan.Steps)
	}
	names := map[string]bool{}
	for i, step := range res.Plan.Steps {
		names[step.Name] = true
		if step.Build == nil || step.Build.Context != "." {
			t.Errorf("step %s builds from %+v, want the repository root", step.Name, step.Build)
		}
		want := "WORKDIR /app/" + step.Name
		if !strings.Contains(step.Dockerfile, want) {
			t.Errorf("step %s does not %s:\n%s", step.Name, want, step.Dockerfile)
		}
		if res.Detected[i].Dir != step.Name {
			t.Errorf("detected dir = %q, want %q", res.Detected[i].Dir, step.Name)
		}
	}
	if !names["web"] || !names["admin"] {
		t.Errorf("steps are %v, want web and admin", names)
	}
}

// A member the detector does not recognise is skipped, not fatal, and the
// skip is named so nobody finds out by counting URLs.
func TestAnUnrecognisedWorkspaceIsSkippedAndNamed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"shop","workspaces":["web","tools"]}`)
	mkdir(t, dir, "web")
	write(t, filepath.Join(dir, "web"), "package.json", `{"name":"web","dependencies":{"@webjsdev/core":"1"}}`)
	mkdir(t, dir, "tools")
	write(t, filepath.Join(dir, "tools"), "package.json", `{"name":"tools"}`)

	res, planErr, unknown, err := Plan(context.Background(), dir, Options{App: "shop"})
	mustPlan(t, res, planErr, unknown, err)
	if len(res.Plan.Steps) != 1 || res.Plan.Steps[0].Name != "web" {
		t.Fatalf("steps = %+v, want web alone", res.Plan.Steps)
	}
	named := false
	for _, n := range res.Detected[0].Notes {
		if strings.Contains(n, "tools") {
			named = true
		}
	}
	if !named {
		t.Errorf("the skipped workspace is not named in the notes: %v", res.Detected[0].Notes)
	}
}

func TestWorkspacesReadsBothSpellings(t *testing.T) {
	for _, field := range []string{`["web"]`, `{"packages":["web"]}`} {
		dir := t.TempDir()
		write(t, dir, "package.json", `{"name":"shop","workspaces":`+field+`}`)
		mkdir(t, dir, "web")
		write(t, filepath.Join(dir, "web"), "package.json", `{"name":"web"}`)
		if got := Workspaces(dir); len(got) != 1 || got[0] != "web" {
			t.Errorf("Workspaces(%s) = %v, want [web]", field, got)
		}
	}
}

func TestAWorkspacePatternThatEscapesTheRootIsDropped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"shop","workspaces":["../outside","/etc"]}`)
	if got := Workspaces(dir); len(got) != 0 {
		t.Errorf("Workspaces = %v, want none: the tar is untrusted", got)
	}
}

// ---------------------------------------------------------------------------

func mustPlan(t *testing.T, res *Result, planErr any, unknown *Unknown, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if unknown != nil {
		t.Fatalf("Plan refused: %v", unknown.Details)
	}
	if res == nil {
		t.Fatalf("Plan returned no result (planErr %v)", planErr)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func rm(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}
