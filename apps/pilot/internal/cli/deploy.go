package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
	"github.com/vivek7405/pilots/cli/internal/out"
)

const (
	preDeployTimeout = 10 * time.Minute
	releaseTimeout   = 5 * time.Minute
)

// composeFileNames is the search order when no --file is given: the same
// four names `docker compose` looks for.
var composeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

func findComposeFile(dir string) string {
	for _, n := range composeFileNames {
		p := filepath.Join(dir, n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// deployedService is one line of the answer: what was deployed, and where.
type deployedService struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	URL     string `json:"url"`
	Release string `json:"release"`
	Build   string `json:"build"`
}

func newDeployCmd(env *Env, getenv config.Env) *cobra.Command {
	var (
		app      string
		envPairs []string
		noWait   bool
		detach   bool
		file     string
		verbose  bool
		ci       bool
	)
	c := &cobra.Command{
		Use:   "deploy [dir]",
		Short: "build a directory and run it as a service: a compose file, a Dockerfile, or neither",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := env.Client()
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			ci = ci || isTruthy(getenv("CI"))
			verbose = verbose || ci
			wait := !noWait && !detach
			interp, err := parseEnv(envPairs)
			if err != nil {
				return err
			}

			// A compose file plans on the host from its text; a plain directory
			// is tarred and planned from what it contains. Either way the
			// host decides, and this side only carries out the plan.
			var (
				plan       pilots.ComposePlan
				composeDir = dir
			)
			composeFile := file
			if composeFile != "" && !filepath.IsAbs(composeFile) {
				composeFile = filepath.Join(dir, composeFile)
			}
			if composeFile == "" {
				composeFile = findComposeFile(dir)
			}
			ctx := c.Context()
			if composeFile != "" {
				text, err := os.ReadFile(composeFile)
				if err != nil {
					return out.Failf("check the path", "read %s: %v", composeFile, err)
				}
				composeDir = filepath.Dir(composeFile)
				p, err := client.Compose.Plan(ctx, pilots.ComposeRequest{Compose: string(text), Env: interp})
				if err != nil {
					return err
				}
				plan = *p
				env.W.Notef("%s: compose file", filepath.Base(composeFile))
			} else {
				tarBytes, err := tarDirectory(dir)
				if err != nil {
					return err
				}
				res, err := client.Plan(ctx, bytes.NewReader(tarBytes), filepath.Base(dir))
				if err != nil {
					return explainUnknown(err)
				}
				for _, d := range res.Detected {
					fw := ""
					if d.Framework != "" {
						fw = " (" + d.Framework + ")"
					}
					env.W.Notef("%s: %s%s in %s", d.Service, d.Source, fw, d.Dir)
				}
				plan = res.Plan
			}
			if app != "" {
				plan.App = app
			}
			env.W.Notef("plan: %d services in app %s", len(plan.Steps), plan.App)

			creds, _ := config.Load(getenv)
			secrets := secretsFor(creds, plan.App)

			results, err := executePlan(ctx, env, client, plan, composeDir, secrets, wait, verbose)
			if err != nil {
				return err
			}
			if env.W.JSON {
				return env.W.JSONValue(results)
			}
			rows := make([][]string, 0, len(results))
			for _, r := range results {
				rows = append(rows, []string{r.Name, r.URL})
			}
			return env.W.Table([]string{"SERVICE", "URL"}, rows)
		},
	}
	f := c.Flags()
	f.StringVar(&app, "app", "", "override the app name the plan derives")
	f.StringArrayVar(&envPairs, "env", nil, "add to the compose interpolation environment, KEY=value (repeatable)")
	f.BoolVar(&noWait, "no-wait", false, "return as soon as each deploy is accepted")
	f.BoolVarP(&detach, "detach", "d", false, "the same as --no-wait")
	f.StringVar(&file, "file", "", "use this compose file instead of searching; relative to [dir]")
	f.BoolVar(&verbose, "verbose", false, "stream every build log line instead of one line per stage")
	f.BoolVarP(&ci, "ci", "c", false, "every log line, no live line; implied by a truthy CI variable")
	Describe(c, Doc{
		What: "One command from a directory to a URL. The host looks at what is\n" +
			"there -- a compose file, a Dockerfile, or a framework it recognises --\n" +
			"and returns a plan; this builds each service on the fleet's layer\n" +
			"cache, cuts a release, and waits for it to pass its health gate.",
		How: "A directory nothing recognises is refused with what was looked for\n" +
			"and what was found, so the fix (usually a Dockerfile) is obvious. A\n" +
			"second deploy of the same directory updates the same services; the\n" +
			"address never changes. Secrets referenced as secret://name resolve\n" +
			"from `pilot secrets` on this machine and never leave it in the clear.",
		Examples: []string{
			"pilot deploy",
			"pilot deploy ./api --app shop",
			"pilot deploy --file deploy/compose.yaml",
			"# for a script or an agent: every line, no waiting on health",
			"pilot deploy -c --no-wait",
		},
		Related: []string{
			"pilot services releases   what each deploy cut",
			"pilot logs                why a replica failed its health gate",
			"pilot promote             the other way to a service, from a machine",
		},
	})
	return c
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// secretsFor reads the app's secrets out of the credentials file: a map of
// app -> name -> value, kept as raw JSON by config so nothing else about the
// file has to be understood here.
func secretsFor(creds *config.Credentials, app string) map[string]string {
	if creds == nil || len(creds.Secrets) == 0 {
		return nil
	}
	var all map[string]map[string]string
	if err := json.Unmarshal(creds.Secrets, &all); err != nil {
		return nil
	}
	return all[app]
}

// resolveSecrets turns a step's secret_refs (env name -> secret name) into
// the sealed environment, refusing a reference nothing on this machine holds:
// deploying with a silently empty DATABASE_URL is worse than not deploying.
func resolveSecrets(refs map[string]string, secrets map[string]string, app string) (map[string]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	sealed := make(map[string]string, len(refs))
	for envName, secretName := range refs {
		v, ok := secrets[secretName]
		if !ok {
			return nil, out.Failf(
				fmt.Sprintf("pilot secrets set %s %s", app, secretName),
				"%s refers to secret://%s, which is not set on this machine", envName, secretName)
		}
		sealed[envName] = v
	}
	return sealed, nil
}

// explainUnknown renders the host's "I do not recognise this directory"
// refusal as what an agent needs to write a Dockerfile: what was looked for,
// what is there, and the rules.
func explainUnknown(err error) error {
	var unknown *pilots.UnknownFramework
	if !errors.As(err, &unknown) {
		return err
	}
	d := unknown.Details
	var b strings.Builder
	fmt.Fprintf(&b, "nothing here is recognised as deployable (%s)\n", d.Dir)
	if len(d.LookedFor) > 0 {
		fmt.Fprintf(&b, "  looked for: %s\n", strings.Join(d.LookedFor, ", "))
	}
	if len(d.Listing) > 0 {
		fmt.Fprintf(&b, "  found:      %s\n", strings.Join(d.Listing, ", "))
	}
	for _, r := range d.Rules {
		fmt.Fprintf(&b, "  - %s\n", r)
	}
	return out.Failf("add a Dockerfile, or a compose file naming one, and deploy again", "%s", strings.TrimRight(b.String(), "\n"))
}

// executePlan walks the plan in the order the host returned it: for each
// step find the existing service, build, ensure its volume, run pre_deploy,
// create or patch the service, deploy, and (unless --no-wait) wait for the
// release to become current.
func executePlan(ctx context.Context, env *Env, client *pilots.Client, plan pilots.ComposePlan, dir string,
	secrets map[string]string, wait, verbose bool) ([]deployedService, error) {

	reporter := newBuildReporter(env, verbose)
	var results []deployedService
	for i := range plan.Steps {
		step := &plan.Steps[i]
		if len(step.Volumes) > 1 {
			return nil, out.Failf("split the service or merge the mounts",
				"%s: declares %d volumes, and a service mounts one", step.Name, len(step.Volumes))
		}
		sealed, err := resolveSecrets(step.SecretRefs, secrets, plan.App)
		if err != nil {
			return nil, err
		}
		existing, err := findService(ctx, client, plan.App, step.Name)
		if err != nil {
			return nil, err
		}
		if err := refuseVolumeChange(ctx, client, plan.App, step, existing); err != nil {
			return nil, err
		}

		env.W.Notef("%s  building", step.Name)
		rootfs, err := buildStep(ctx, client, step, dir, reporter)
		if err != nil {
			return nil, err
		}
		env.W.Notef("%s  built %s", step.Name, rootfs)

		volumeID, err := ensureVolume(ctx, client, plan.App, step)
		if err != nil {
			return nil, err
		}
		if err := runPreDeploy(ctx, env, client, plan.App, step, rootfs, sealed); err != nil {
			return nil, err
		}
		service, err := upsertService(ctx, client, plan.App, step, rootfs, sealed, existing, volumeID)
		if err != nil {
			return nil, err
		}
		release, err := client.Services.Deploy(ctx, service.ID, pilots.DeployRequest{Build: rootfs, Knobs: step.Knobs})
		if err != nil {
			return nil, err
		}
		env.W.Notef("%s  release %s accepted", step.Name, release.ID)
		if wait {
			started := time.Now()
			if err := waitForRelease(ctx, client, service.ID, release.ID); err != nil {
				return nil, err
			}
			env.W.Notef("%s  release %s is current in %.1fs", step.Name, release.ID, time.Since(started).Seconds())
		}
		current, err := client.Services.Get(ctx, service.ID)
		if err != nil {
			return nil, err
		}
		results = append(results, deployedService{
			Name: current.Name, ID: current.ID, URL: serviceAddress(current), Release: release.ID, Build: rootfs,
		})
	}
	return results, nil
}

func findService(ctx context.Context, client *pilots.Client, app, name string) (*pilots.Service, error) {
	services, err := client.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range services {
		if services[i].Name == name && services[i].App == app {
			return &services[i], nil
		}
	}
	return nil, nil
}

// refuseVolumeChange: a service's volume is set when it is created, so a
// compose file that names a different one than the service mounts is a
// mistake to refuse, not a migration to attempt.
func refuseVolumeChange(ctx context.Context, client *pilots.Client, app string, step *pilots.ComposeStep, existing *pilots.Service) error {
	if existing == nil {
		return nil
	}
	wantName := ""
	if len(step.Volumes) == 1 {
		wantName = app + "-" + step.Volumes[0].Name
	}
	mounted := existing.VolumeID
	if mounted == "" && wantName == "" {
		return nil
	}
	volumes, err := client.Volumes.List(ctx)
	if err != nil {
		return err
	}
	byName, byID := map[string]string{}, map[string]string{}
	for _, v := range volumes {
		byName[v.Name] = v.ID
		byID[v.ID] = v.Name
	}
	if mounted != "" && wantName != "" && byName[wantName] == mounted {
		return nil
	}
	mountedName := byID[mounted]
	if mountedName == "" {
		mountedName = mounted
	}
	if mountedName == "" {
		mountedName = "(no volume)"
	}
	if wantName == "" {
		wantName = "(no volume)"
	}
	return out.Failf("a service's volume is set when it is created",
		"%s: mounts %s and the compose file names %s", step.Name, mountedName, wantName)
}

// buildStep packs the step's context and streams the build. The plan's
// Dockerfile wins over the one on disk: it is the version the host decided
// to build, with the plan's overrides applied.
func buildStep(ctx context.Context, client *pilots.Client, step *pilots.ComposeStep, dir string, rep *buildReporter) (string, error) {
	var tarBytes []byte
	var err error
	switch {
	case step.Build != nil:
		contextDir := dir
		if step.Build.Context != "" {
			contextDir = filepath.Join(dir, step.Build.Context)
		}
		extra := map[string]string{}
		switch {
		case step.Dockerfile != "":
			extra["Dockerfile"] = withOverrides(step.Dockerfile, step)
		case (step.Build.Dockerfile != "" && step.Build.Dockerfile != "Dockerfile") || step.DockerfileAppend != "":
			named := step.Build.Dockerfile
			if named == "" {
				named = "Dockerfile"
			}
			raw, err := os.ReadFile(filepath.Join(contextDir, named))
			if err != nil {
				return "", out.Failf("check the compose file's build.dockerfile", "read %s: %v", named, err)
			}
			extra["Dockerfile"] = withOverrides(string(raw), step)
		}
		if len(extra) == 0 {
			extra = nil
		}
		tarBytes, err = tarDirectoryWith(contextDir, extra)
	case step.Dockerfile != "":
		tarBytes, err = tarFiles(map[string]string{"Dockerfile": step.Dockerfile})
	default:
		return "", out.Failf("this is a host-side plan defect worth reporting",
			"%s: the plan carries neither a build context nor a Dockerfile", step.Name)
	}
	if err != nil {
		return "", err
	}
	stream, err := client.Builds.Create(ctx, bytes.NewReader(tarBytes), pilots.BuildOptions{})
	if err != nil {
		return "", err
	}
	for line, err := range stream.Lines {
		if err != nil {
			return "", err
		}
		rep.line(step.Name, line)
	}
	rootfs, err := stream.Result()
	if err != nil {
		return "", err
	}
	return rootfs, nil
}

// withOverrides applies what the plan adds to a Dockerfile.
func withOverrides(dockerfile string, step *pilots.ComposeStep) string {
	if step.DockerfileAppend == "" {
		return dockerfile
	}
	return strings.TrimRight(dockerfile, "\n") + "\n" + strings.TrimRight(step.DockerfileAppend, "\n") + "\n"
}

// ensureVolume finds or creates the step's volume, named <app>-<volume> so
// two apps declaring `data` do not share one.
func ensureVolume(ctx context.Context, client *pilots.Client, app string, step *pilots.ComposeStep) (string, error) {
	if len(step.Volumes) == 0 {
		return "", nil
	}
	want := step.Volumes[0]
	name := app + "-" + want.Name
	existing, err := client.Volumes.List(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range existing {
		if v.Name == name {
			return v.ID, nil
		}
	}
	v, err := client.Volumes.Create(ctx, pilots.CreateVolumeRequest{Name: name, SizeGiB: want.SizeGiB, MountPath: want.MountPath})
	if err != nil {
		return "", err
	}
	return v.ID, nil
}

// runPreDeploy runs the step's pre_deploy command (a migration, usually) on
// a throwaway machine from the new build, before any replica is replaced.
// The machine is destroyed whatever happens.
func runPreDeploy(ctx context.Context, env *Env, client *pilots.Client, app string, step *pilots.ComposeStep, rootfs string, sealed map[string]string) error {
	if step.PreDeploy == "" {
		return nil
	}
	off, no := "off", false
	m, err := client.Machines.Create(ctx, pilots.CreateMachineRequest{
		Name:  fmt.Sprintf("%s-%s-predeploy-%d", app, step.Name, time.Now().Unix()),
		Image: rootfs, App: app, Env: step.Env, SecretEnv: sealed,
		Knobs: &pilots.KnobsPatch{AutoStop: &off, AutoStart: &no},
	})
	if err != nil {
		return err
	}
	defer func() { _ = client.Machines.Destroy(context.WithoutCancel(ctx), m.ID) }()

	env.W.Notef("%s  pre_deploy: %s", step.Name, step.PreDeploy)
	res, err := client.Machines.Exec(ctx, m.ID, pilots.ExecRequest{
		Cmd: step.PreDeploy, Cwd: "/app", Env: step.Env, TimeoutMS: int(preDeployTimeout.Milliseconds()),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		fmt.Fprint(os.Stderr, res.Stdout, res.Stderr)
		return out.Failf("fix the command and deploy again; no replica was replaced",
			"%s: pre_deploy exited %d", step.Name, res.ExitCode)
	}
	return nil
}

func upsertService(ctx context.Context, client *pilots.Client, app string, step *pilots.ComposeStep, rootfs string,
	sealed map[string]string, existing *pilots.Service, volumeID string) (*pilots.Service, error) {
	if existing == nil {
		return client.Services.Create(ctx, pilots.CreateServiceRequest{
			Name: step.Name, App: app, Build: rootfs, Replicas: step.Replicas,
			Health: step.Health, Domain: step.Domain, Private: step.Private, CustomDomain: step.CustomDomain,
			Volume: volumeID, Env: step.Env, SecretEnv: sealed, Knobs: step.Knobs,
		})
	}
	replicas := step.Replicas
	return client.Services.Patch(ctx, existing.ID, pilots.UpdateServiceRequest{
		Replicas: &replicas, Health: step.Health, Env: step.Env, SecretEnv: sealed,
	})
}

// waitForRelease polls until the release is current. The host flips
// release_id only once the new replicas pass the health gate, so "current"
// is the whole verdict; a gate failure surfaces from Deploy itself.
func waitForRelease(ctx context.Context, client *pilots.Client, serviceID, releaseID string) error {
	deadline := time.Now().Add(releaseTimeout)
	for {
		s, err := client.Services.Get(ctx, serviceID)
		if err != nil {
			return err
		}
		if s.ReleaseID == releaseID {
			return nil
		}
		if time.Now().After(deadline) {
			return out.Failf("pilot services releases <name> shows whether it went healthy; pilot logs <name> shows the replicas' output",
				"release %s did not become current within %s", releaseID, releaseTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// buildReporter renders build log lines: every line under --verbose, and
// otherwise one line per stage so a deploy reads as a list of steps rather
// than a wall of layer output. Status lines and errors always show.
type buildReporter struct {
	env     *Env
	verbose bool
	tty     bool
	live    bool
}

func newBuildReporter(env *Env, verbose bool) *buildReporter {
	return &buildReporter{env: env, verbose: verbose, tty: term.IsTerminal(int(os.Stderr.Fd()))}
}

func (r *buildReporter) line(step string, l pilots.BuildLogLine) {
	text := strings.TrimRight(l.Line, "\n")
	switch {
	case l.Error != "":
		r.clear()
		fmt.Fprintf(os.Stderr, "%s  %s\n", step, l.Error)
	case l.Stream == "status":
		r.clear()
		fmt.Fprintf(os.Stderr, "%s  %s\n", step, text)
	case r.verbose:
		fmt.Fprintf(os.Stderr, "%s  %s\n", step, text)
	case r.tty && text != "":
		// One live line, overwritten in place, so progress is visible
		// without scrolling the terminal.
		fmt.Fprintf(os.Stderr, "\r\033[K%s  %s", step, truncate(text, 100))
		r.live = true
	}
}

func (r *buildReporter) clear() {
	if r.live {
		fmt.Fprint(os.Stderr, "\r\033[K")
		r.live = false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
