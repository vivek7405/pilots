package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vivek7405/pilots/agents"
	pilotsmcp "github.com/vivek7405/pilots/agents/mcp"
	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
)

// The MCP server over stdio: the fleet toolset every host also serves at
// /mcp (agents/mcp), plus the tools that need THIS machine's filesystem --
// a directory to tar, a file to push or pull. The hosted endpoint cannot
// offer those, which is the whole reason `pilot mcp` still exists.
//
// STDOUT IS THE PROTOCOL CHANNEL. Over stdio the client parses every byte
// this process writes to stdout as JSON-RPC, so nothing here may print to
// it; the logger and every diagnostic go to stderr.

type mcpDeps struct {
	client *pilots.Client
	getenv config.Env
	env    *Env
	pages  []agents.Page
}

func buildMCPServer(d mcpDeps) *mcp.Server {
	s := pilotsmcp.NewServer(d.client, pilotsmcp.Options{Version: Version, Pages: d.pages, Local: true})
	d.registerLocalTools(s)
	return s
}

// registerLocalTools adds pilotsmcp.LocalTools: the ones that read or write
// the agent's own disk.
func (d mcpDeps) registerLocalTools(s *mcp.Server) {
	client := d.client
	wrap, constant, toObject := pilotsmcp.Wrap, pilotsmcp.Constant, pilotsmcp.ToObject

	type buildIn struct {
		Dir        string `json:"dir" jsonschema:"the build context directory"`
		Dockerfile string `json:"dockerfile,omitempty" jsonschema:"Dockerfile CONTENTS (not a path); replaces any Dockerfile in the directory"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "build", Title: "Build a rootfs",
		Description: "Tar a directory and build it into a bootable root filesystem, returning a rootfs build id for deploy. " +
			"Pass `dockerfile` to build with a Dockerfile you wrote here, without writing it to disk first. " +
			"On failure the result carries EVERY log line as NDJSON: read the failing step, fix the Dockerfile, call this again. " +
			pilotsmcp.DockerfileRules},
		func(ctx context.Context, _ *mcp.CallToolRequest, in buildIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				var extra map[string]string
				if in.Dockerfile != "" {
					extra = map[string]string{"Dockerfile": in.Dockerfile}
				}
				tarBytes, err := tarDirectoryWith(in.Dir, extra)
				if err != nil {
					return nil, err
				}
				stream, err := client.Builds.Create(ctx, bytes.NewReader(tarBytes), pilots.BuildOptions{})
				if err != nil {
					return nil, err
				}
				rootfs, err := stream.Result()
				if err != nil {
					return nil, err
				}
				n := 0
				for range stream.Lines {
					n++
				}
				return map[string]any{"rootfs_build_id": rootfs, "build_id": stream.ID, "steps": n}, nil
			}, func(r any) string {
				if m, ok := toObject(r); ok {
					return fmt.Sprintf("deploy with name and build=%v", m["rootfs_build_id"])
				}
				return ""
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "deploy", Title: "Deploy",
		Description: "Deploy a directory to a URL in one call: plans it on the host, builds each service, deploys, " +
			"waits for the health gate, and returns { app, services: [{ name, url }], next }. " +
			"Call this FIRST for any \"deploy this\" request, with `dir` and nothing else unless the user gave more. " +
			"No directory and no repository in the conversation: ask, never invent a path. " +
			"Also accepts `name` + `build` for a rootfs you built yourself. " +
			"On unknown_framework read `details` and call `build` with a Dockerfile you write; " +
			"on health_gate_failed call `diagnose` with `details.replica`."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deployIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				if in.Dir != "" {
					return d.deployDir(ctx, in)
				}
				if in.Name == "" || in.Build == "" {
					return nil, errors.New("pass dir to deploy a directory, or name and build to deploy a rootfs you already built")
				}
				return d.deployBuild(ctx, in)
			}, constant("report the URL; the deploy is done"))
		})

	type genIn struct {
		Dir   string `json:"dir"`
		Write bool   `json:"write,omitempty" jsonschema:"also write the Dockerfile, if the directory has none"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_dockerfile", Title: "Generate a Dockerfile",
		Description: "Detect the framework in a directory and return a Dockerfile for it, with the port and health check to deploy it with. " +
			"Use this before `build` on a repo that has no Dockerfile. " + pilotsmcp.DockerfileRules},
		func(ctx context.Context, _ *mcp.CallToolRequest, in genIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				tarBytes, err := tarDirectory(in.Dir)
				if err != nil {
					return nil, err
				}
				res, err := client.Plan(ctx, bytes.NewReader(tarBytes), filepath.Base(in.Dir))
				if err != nil {
					return nil, err
				}
				type recipe struct {
					Service    string              `json:"service"`
					Framework  string              `json:"framework"`
					Dir        string              `json:"dir"`
					Dockerfile string              `json:"dockerfile"`
					Port       int                 `json:"port"`
					Health     *pilots.HealthCheck `json:"health,omitempty"`
					Notes      []string            `json:"notes"`
				}
				var recipes []recipe
				for i, det := range res.Detected {
					if det.Source != "recipe" {
						continue
					}
					df := ""
					if i < len(res.Plan.Steps) {
						df = res.Plan.Steps[i].Dockerfile
					}
					notes := det.Notes
					if notes == nil {
						notes = []string{}
					}
					recipes = append(recipes, recipe{det.Service, det.Framework, det.Dir, df, det.Port, det.Health, notes})
				}
				if len(recipes) == 0 {
					return nil, errors.New("this directory already has a compose file or a Dockerfile, so the platform " +
						"would build that rather than generate one; deploy it with `deploy`")
				}
				if in.Write {
					path := filepath.Join(in.Dir, "Dockerfile")
					_, statErr := os.Stat(path)
					existed := statErr == nil
					written := false
					if !existed && len(recipes) == 1 {
						if err := os.WriteFile(path, []byte(recipes[0].Dockerfile), 0o644); err != nil {
							return nil, err
						}
						written = true
					}
					return map[string]any{"recipes": recipes, "written": written, "path": path}, nil
				}
				return map[string]any{"recipes": recipes}, nil
			}, constant("deploy with the same dir"))
		})

	type planIn struct {
		Dir string `json:"dir" jsonschema:"the directory to plan"`
		App string `json:"app,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "plan", Title: "Plan a directory",
		Description: "Show what `deploy` would do without building anything: the services, the source of each " +
			"(compose, dockerfile, recipe), the ports, the health checks, and any generated Dockerfile. " +
			"Call it when asked what will happen, or to check how a monorepo splits before a build. " +
			"Next: deploy with the same dir."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in planIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				tarBytes, err := tarDirectory(in.Dir)
				if err != nil {
					return nil, err
				}
				app := in.App
				if app == "" {
					app = filepath.Base(in.Dir)
				}
				return client.Plan(ctx, bytes.NewReader(tarBytes), app)
			}, constant("deploy with dir="+in.Dir))
		})

	type pushIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Src     string `json:"src" jsonschema:"a local file or directory"`
		Dest    string `json:"dest" jsonschema:"the path it lands at on the machine"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "push_file", Title: "Copy a file into a machine",
		Description: "Copy a local file or directory into a machine at dest, over the exec stream. " +
			"A directory lands as dest itself. Next: exec to use it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pushIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := pilotsmcp.ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				if err := pushFiles(ctx, client, m.ID, in.Src, in.Dest); err != nil {
					return nil, err
				}
				return map[string]string{"machine": m.ID, "dest": in.Dest}, nil
			}, constant("exec on the machine to use it"))
		})

	type pullIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Src     string `json:"src" jsonschema:"a file or directory on the machine"`
		Dest    string `json:"dest" jsonschema:"where it lands locally; an existing directory receives it inside"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "pull_file", Title: "Copy a file out of a machine",
		Description: "Copy a file or directory out of a machine to a local path, over the exec stream. " +
			"For a log, `logs` is cheaper; this is for build output, data files, anything you want on disk here."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pullIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := pilotsmcp.ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				if err := pullFiles(ctx, client, m.ID, in.Src, in.Dest); err != nil {
					return nil, err
				}
				return map[string]string{"machine": m.ID, "src": in.Src, "dest": in.Dest}, nil
			}, constant(""))
		})
}

// deployDir is the dir form of the deploy tool: the same plan and pipeline
// the CLI runs, with a caller's per-service overrides applied, and refused
// where they cannot be applied unambiguously.
func (d mcpDeps) deployDir(ctx context.Context, in deployIn) (any, error) {
	tarBytes, err := tarDirectory(in.Dir)
	if err != nil {
		return nil, err
	}
	res, err := d.client.Plan(ctx, bytes.NewReader(tarBytes), filepath.Base(in.Dir))
	if err != nil {
		return nil, err
	}
	plan := res.Plan
	if in.App != "" {
		plan.App = in.App
	}
	if err := applyOverrides(&plan, in); err != nil {
		return nil, err
	}
	creds, _ := config.Load(d.getenv)
	results, err := executePlan(ctx, d.env, d.client, plan, in.Dir, secretsFor(creds, plan.App), true, false)
	if err != nil {
		return nil, err
	}
	services := make([]map[string]string, 0, len(results))
	for _, r := range results {
		services = append(services, map[string]string{"name": r.Name, "url": r.URL, "release_id": r.Release, "service_id": r.ID})
	}
	return map[string]any{"app": plan.App, "services": services}, nil
}

// deployIn is shared by deployDir/deployBuild and applyOverrides.
type deployIn = struct {
	Dir          string              `json:"dir,omitempty" jsonschema:"the directory to deploy; the host decides what it is"`
	Name         string              `json:"name,omitempty" jsonschema:"the service name, for the name + build form"`
	Build        string              `json:"build,omitempty" jsonschema:"a rootfs build id, for the name + build form"`
	App          string              `json:"app,omitempty"`
	Port         int                 `json:"port,omitempty" jsonschema:"sets PORT in the service environment"`
	Domain       string              `json:"domain,omitempty"`
	Private      bool                `json:"private,omitempty" jsonschema:"mint no address; peers still reach it at <name>.internal"`
	CustomDomain string              `json:"custom_domain,omitempty"`
	Health       *pilots.HealthCheck `json:"health,omitempty"`
	Env          map[string]string   `json:"env,omitempty"`
	SecretEnv    map[string]string   `json:"secret_env,omitempty"`
	Replicas     int                 `json:"replicas,omitempty"`
}

// applyOverrides: a health or replicas passed alongside dir and then quietly
// dropped is the worst outcome available -- the deploy succeeds, the gate
// polls something else, and nothing says the argument was ignored. So it is
// applied, and where it cannot be unambiguously (a monorepo) it is refused
// with the fix named.
func applyOverrides(plan *pilots.ComposePlan, in deployIn) error {
	var given []string
	if in.Port != 0 {
		given = append(given, "port")
	}
	if in.Health != nil {
		given = append(given, "health")
	}
	if in.Env != nil {
		given = append(given, "env")
	}
	if in.SecretEnv != nil {
		given = append(given, "secret_env")
	}
	if in.Replicas != 0 {
		given = append(given, "replicas")
	}
	if in.Domain != "" {
		given = append(given, "domain")
	}
	if in.CustomDomain != "" {
		given = append(given, "custom_domain")
	}
	if len(given) == 0 {
		return nil
	}
	if len(plan.Steps) != 1 {
		return fmt.Errorf("this directory plans %d services, so %s cannot be applied to one of them; put them in a compose file",
			len(plan.Steps), strings.Join(given, ", "))
	}
	step := &plan.Steps[0]
	if in.Port != 0 || in.Env != nil {
		env := map[string]string{}
		for k, v := range step.Env {
			env[k] = v
		}
		if in.Port != 0 {
			env["PORT"] = fmt.Sprint(in.Port)
		}
		for k, v := range in.Env {
			env[k] = v
		}
		step.Env = env
	}
	if in.Health != nil {
		step.Health = in.Health
	}
	if in.Replicas != 0 {
		step.Replicas = in.Replicas
	}
	if in.Domain != "" {
		step.Domain = in.Domain
	}
	if in.Private {
		step.Private = true
	}
	if in.CustomDomain != "" {
		step.CustomDomain = in.CustomDomain
	}
	if in.SecretEnv != nil {
		return errors.New("secret_env with dir is not supported: put secret:// references in a compose file, or deploy with name and build")
	}
	return nil
}

// deployBuild is the name + build form: a rootfs the caller built itself.
func (d mcpDeps) deployBuild(ctx context.Context, in deployIn) (any, error) {
	env := in.Env
	if in.Port != 0 {
		env = map[string]string{}
		for k, v := range in.Env {
			env[k] = v
		}
		env["PORT"] = fmt.Sprint(in.Port)
	}
	existing, err := findService(ctx, d.client, in.App, in.Name)
	if err != nil {
		return nil, err
	}
	var service *pilots.Service
	if existing != nil {
		req := pilots.UpdateServiceRequest{Health: in.Health, Env: env, SecretEnv: in.SecretEnv}
		if in.Replicas != 0 {
			r := in.Replicas
			req.Replicas = &r
		}
		service, err = d.client.Services.Patch(ctx, existing.ID, req)
	} else {
		service, err = d.client.Services.Create(ctx, pilots.CreateServiceRequest{
			Name: in.Name, Build: in.Build, App: in.App, Replicas: in.Replicas, Health: in.Health,
			Domain: in.Domain, Private: in.Private, CustomDomain: in.CustomDomain, Env: env, SecretEnv: in.SecretEnv,
		})
	}
	if err != nil {
		return nil, err
	}
	release, err := d.client.Services.Deploy(ctx, service.ID, pilots.DeployRequest{Build: in.Build})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Minute)
	for {
		current, err := d.client.Services.Get(ctx, service.ID)
		if err != nil {
			return nil, err
		}
		if current.ReleaseID == release.ID {
			return map[string]string{"service_id": service.ID, "url": serviceAddress(current), "release_id": release.ID}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("release %s did not become current within 600s", release.ID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// runMCP serves the fleet over stdio until the client goes away.
func runMCP(ctx context.Context, d mcpDeps) error {
	// Belt and braces for the protocol channel: anything in this process that
	// still reaches os.Stdout after this point would corrupt a frame, so the
	// package-level writer commands print through is pointed at stderr.
	d.env.W.Out = os.Stderr
	err := buildMCPServer(d).Run(ctx, &mcp.StdioTransport{})
	// The client closing its end is how a session ends, not a failure: an
	// agent that exits leaves stdin at EOF, and reporting that as an error
	// would put a spurious "error:" in the last thing its log shows.
	if err == nil || errors.Is(err, io.EOF) || strings.Contains(err.Error(), "server is closing") {
		return nil
	}
	return err
}
