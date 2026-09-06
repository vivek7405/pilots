package detect

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/compose"
)

// composeNames is the order the CLI's finder uses and the order docker compose
// settled on. First hit wins rather than "merge them all", so a leftover
// docker-compose.yml beside a current compose.yaml cannot silently deploy the
// wrong thing.
var composeNames = []string{
	"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml",
}

// manifestNames are the dependency files an unknown answer carries back, so an
// agent writing a Dockerfile can see what the project declares without a
// round trip to read files it has to guess the names of.
var manifestNames = []string{
	"package.json", "pyproject.toml", "requirements.txt",
	"go.mod", "Gemfile", "composer.json", "Cargo.toml",
}

const (
	maxManifestBytes = 16 << 10
	maxListing       = 200
)

// Options are what the request said about the directory, as opposed to what
// the directory says about itself.
type Options struct {
	// App is ?app=, falling back to package.json's name and then "app".
	App string
	// Env is the tar's own .env, the interpolation map for a compose file.
	Env map[string]string
}

// Result is a resolved directory: the plan an executor runs, and one Detected
// per step saying how the planner decided it.
type Result struct {
	Plan     compose.Plan
	Detected []compose.Detected
}

// Unknown is the structured refusal.
//
// An error rather than a third return value with no type, so the push path can
// errors.As it out of a call chain that is otherwise about builds.
type Unknown struct{ Details compose.UnknownDetails }

func (u *Unknown) Error() string {
	return "no framework was detected and there is no Dockerfile or compose file"
}

// Plan resolves a directory. Exactly one of the four returns is non-nil.
//
// The order is the whole design and it is: a compose file, then a Dockerfile,
// then a recipe, then unknown. A file the repository wrote beats a file the
// platform would write, always. The platform's guess is good and the author's
// file is a decision, and a platform that overrides a decision because its
// guess looked confident is one nobody can predict.
func Plan(ctx context.Context, dir string, opts Options) (*Result, *compose.PlanError, *Unknown, error) {
	app := opts.App
	if app == "" {
		app = appNameFromPackage(dir)
	}
	if app == "" {
		app = "app"
	}

	// (a) a compose file at the root.
	if file := findCompose(dir); file != "" {
		text, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, nil, err
		}
		env := map[string]string{}
		for k, v := range opts.Env {
			env[k] = v
		}
		if env["COMPOSE_PROJECT_NAME"] == "" {
			env["COMPOSE_PROJECT_NAME"] = app
		}
		plan, planErr, err := compose.Compile(ctx, compose.Request{
			Compose: string(text), Env: env,
		})
		if planErr != nil || err != nil {
			return nil, planErr, nil, err
		}
		detected := make([]compose.Detected, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			detected = append(detected, compose.Detected{
				Service: step.Name, Source: "compose",
				Dir: contextOf(step), Port: portOf(step), Health: step.Health,
			})
		}
		return &Result{Plan: *plan, Detected: detected}, nil, nil, nil
	}

	// (b) a Dockerfile at the root. Health is left nil so the rollout applies
	// its own defaults; parsing a HEALTHCHECK out of the file is a separate
	// decision and guessing one here would override what the author wrote.
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		step := baseStep("web")
		step.Build = &compose.Build{Context: "."}
		return &Result{
			Plan: compose.Plan{App: app, Steps: []compose.Step{step}},
			Detected: []compose.Detected{{
				Service: "web", Source: "dockerfile", Dir: ".", Port: AppPort,
			}},
		}, nil, nil, nil
	}

	// (c) a recipe for the root itself, unless the root declares workspaces.
	//
	// The exclusion is what makes the monorepo case work at all. A workspace
	// root hoists its members' dependencies, so it looks exactly like an app
	// to a detector reading package.json: the webjs monorepo's root declares
	// @webjsdev/* and is not a webjs app. Declaring workspaces is the author
	// saying the apps are underneath, so the root is not one of them.
	members := Workspaces(dir)
	if recipe, ok := Generate(dir); ok && len(members) == 0 {
		step := baseStep("web")
		step.Build = &compose.Build{Context: "."}
		step.Dockerfile = recipe.Dockerfile
		step.Health = recipe.Health
		return &Result{
			Plan: compose.Plan{App: app, Steps: []compose.Step{step}},
			Detected: []compose.Detected{{
				Service: "web", Source: "recipe", Framework: string(recipe.Framework),
				Dir: ".", Port: recipe.Port, Health: recipe.Health, Notes: recipe.Notes,
			}},
		}, nil, nil, nil
	}

	// (d) npm workspaces: one service per member the detector recognises.
	if res := planWorkspaces(dir, app, members); res != nil {
		return res, nil, nil, nil
	}

	return nil, nil, &Unknown{Details: unknownDetails(dir, members)}, nil
}

// planWorkspaces turns the recognised members into steps, or nil when none
// were recognised.
//
// A member the detector does not know is skipped rather than failing the whole
// repository: a monorepo with three apps and one unrecognised tools directory
// should deploy the three, and the skip is named in the first step's notes so
// nobody discovers it by counting URLs.
func planWorkspaces(dir, app string, members []string) *Result {
	var steps []compose.Step
	var detected []compose.Detected
	var skipped []string

	for _, rel := range members {
		recipe, ok := Generate(filepath.Join(dir, rel))
		if !ok {
			skipped = append(skipped, rel)
			continue
		}
		member := recipe.ForWorkspace(rel)
		name := filepath.Base(rel)
		step := baseStep(name)
		// The context is the repository root, never the member's directory:
		// the install is the root's, so the lockfile and the hoisted
		// node_modules are the ones the workspace expects.
		step.Build = &compose.Build{Context: "."}
		step.Dockerfile = member.Dockerfile
		step.Health = member.Health
		steps = append(steps, step)
		detected = append(detected, compose.Detected{
			Service: name, Source: "recipe", Framework: string(member.Framework),
			Dir: rel, Port: member.Port, Health: member.Health, Notes: member.Notes,
		})
	}
	if len(steps) == 0 {
		return nil
	}
	if len(skipped) > 0 {
		detected[0].Notes = append(detected[0].Notes,
			"no framework was detected in these workspaces, so they are not deployed: "+
				strings.Join(skipped, ", "))
	}
	return &Result{Plan: compose.Plan{App: app, Steps: steps}, Detected: detected}
}

// baseStep is a step with the compose planner's own defaults, read through the
// exported constants so the two paths agree by construction: a directory with
// a compose file and the same directory without one must not produce services
// of different sizes.
func baseStep(name string) compose.Step {
	return compose.Step{
		Name:     name,
		Ports:    []int{AppPort},
		Env:      map[string]string{"PORT": "8080"},
		Replicas: compose.DefaultReplicas,
		VCPUs:    compose.DefaultVCPUs,
		MemMiB:   compose.DefaultMemMiB,
	}
}

func findCompose(dir string) string {
	for _, name := range composeNames {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func contextOf(step compose.Step) string {
	if step.Build != nil && step.Build.Context != "" {
		return step.Build.Context
	}
	return "."
}

func portOf(step compose.Step) int {
	if len(step.Ports) > 0 {
		return step.Ports[0]
	}
	return AppPort
}

func appNameFromPackage(dir string) string {
	pkg := readPackageJSON(dir)
	if pkg == nil {
		return ""
	}
	name, _ := pkg["name"].(string)
	// A scoped name is not a legal app name, and the scope is the publisher's
	// rather than the app's, so only the last segment survives.
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// unknownDetails is everything an agent needs to write the Dockerfile itself.
//
// The listing and the manifests are here because the alternative is a model
// calling a file-reading tool five times to guess at names, and three of those
// calls are always wrong. Capped, because the point is the dependency list and
// not a lockfile.
func unknownDetails(dir string, workspaces []string) compose.UnknownDetails {
	entries := listing(dir)
	if len(entries) > maxListing {
		entries = entries[:maxListing]
	}
	manifests := map[string]string{}
	for _, name := range manifestNames {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if len(raw) > maxManifestBytes {
			raw = raw[:maxManifestBytes]
		}
		manifests[name] = string(raw)
	}
	if len(manifests) == 0 {
		manifests = nil
	}
	sort.Strings(workspaces)
	return compose.UnknownDetails{
		Dir: ".", LookedFor: LookedFor, Listing: entries,
		Manifests: manifests, Workspaces: workspaces, Rules: Rules,
	}
}
