package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pilots "github.com/vivek7405/pilots/sdks/go"

	"github.com/vivek7405/pilots/cli/internal/config"
)

// The MCP server: the same fleet, for an agent. Every tool answers JSON with
// a `next` folded in, so a tool cannot ship without saying what to do after
// it; every failure is isError with the most actionable text available --
// the server's own body for an API error (it already carries code, next and
// details), every NDJSON line for a failed build, the message otherwise.
//
// STDOUT IS THE PROTOCOL CHANNEL. Over stdio the client parses every byte
// this process writes to stdout as JSON-RPC, so nothing here may print to
// it; the logger and every diagnostic go to stderr.

const dockerfileRules = "Any Dockerfile you write must bind 0.0.0.0 (never 127.0.0.1, which serves only the guest itself) " +
	"and read the port from $PORT. Both mistakes produce a build that succeeds and a URL that answers 502."

type mcpDeps struct {
	client *pilots.Client
	getenv config.Env
	env    *Env
	skill  string
}

func buildMCPServer(d mcpDeps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "pilots", Version: Version}, nil)
	d.registerTools(s)
	d.registerSkill(s)
	d.registerPrompts(s)
	return s
}

// wrap shapes a handler's answer. next is a function of the result where
// the next step depends on what came back, and a constant otherwise.
func wrap(fn func() (any, error), next func(any) string) (*mcp.CallToolResult, any, error) {
	result, err := fn()
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errorText(err)}}}, nil, nil
	}
	step := ""
	if next != nil {
		step = next(result)
	}
	var body any
	if m, ok := toObject(result); ok {
		m["next"] = step
		body = m
	} else {
		body = map[string]any{"result": result, "next": step}
	}
	text, _ := json.MarshalIndent(body, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil, nil
}

func constant(next string) func(any) string { return func(any) string { return next } }

// toObject round-trips a struct through JSON so `next` can be added beside
// its fields rather than wrapping it.
func toObject(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

func errorText(err error) string {
	var bf *pilots.BuildFailed
	if errors.As(err, &bf) && len(bf.Lines) > 0 {
		var b strings.Builder
		for _, l := range bf.Lines {
			raw, _ := json.Marshal(l)
			b.Write(raw)
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n")
	}
	var apiErr *pilots.Error
	if errors.As(err, &apiErr) {
		if raw, e := json.Marshal(apiErr); e == nil {
			return string(raw)
		}
	}
	return err.Error()
}

func idOf(v any) string {
	if m, ok := toObject(v); ok {
		if id, ok := m["id"].(string); ok {
			return id
		}
	}
	return ""
}

type machineIn struct {
	Machine string `json:"machine" jsonschema:"a machine id or name"`
}

func (d mcpDeps) registerTools(s *mcp.Server) {
	client := d.client

	type createIn struct {
		Name       string            `json:"name,omitempty" jsonschema:"a stable name; the URL is derived from it and never changes"`
		Image      string            `json:"image,omitempty" jsonschema:"a rootfs build id from the build tool"`
		Template   string            `json:"template,omitempty"`
		Checkpoint string            `json:"checkpoint,omitempty" jsonschema:"restore this checkpoint into the new machine"`
		VCPUs      int               `json:"vcpus,omitempty"`
		MemMiB     int               `json:"mem_mib,omitempty"`
		App        string            `json:"app,omitempty"`
		Cmd        string            `json:"cmd,omitempty" jsonschema:"the start command, overriding the image"`
		Env        map[string]string `json:"env,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "create_machine", Title: "Create a machine",
		Description: "Create a microVM. A create is a restore from a template rather than a boot, so it is fast. " +
			"The same primitive serves both a throwaway sandbox and a production replica; only the lifecycle knobs differ."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				return client.Machines.Create(ctx, pilots.CreateMachineRequest{
					Name: in.Name, Image: in.Image, Template: in.Template, Checkpoint: in.Checkpoint,
					VCPUs: in.VCPUs, MemMiB: in.MemMiB, App: in.App, Cmd: in.Cmd, Env: in.Env,
				})
			}, constant("exec on the returned id"))
		})

	type listIn struct {
		App string `json:"app,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "list_machines", Title: "List machines",
		Description: "Every machine this API key can see, optionally narrowed to one app."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				all, err := client.Machines.List(ctx)
				if err != nil || in.App == "" {
					return all, err
				}
				kept := []pilots.Machine{}
				for _, m := range all {
					if m.App == in.App {
						kept = append(kept, m)
					}
				}
				return kept, nil
			}, constant(""))
		})

	type statusIn struct {
		Machine string `json:"machine,omitempty" jsonschema:"a machine id or name"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "status", Title: "Status",
		Description: "With a machine, that machine. Without one, the fleet: every host as the answering host sees it, " +
			"plus a count of machines by state."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				if in.Machine != "" {
					return resolveMachine(ctx, client, in.Machine)
				}
				hosts, err := client.Hosts.List(ctx)
				if err != nil {
					return nil, err
				}
				machines, err := client.Machines.List(ctx)
				if err != nil {
					return nil, err
				}
				byState := map[string]int{}
				for _, m := range machines {
					byState[m.State]++
				}
				return map[string]any{"hosts": hosts, "machines_by_state": byState, "machines_total": len(machines)}, nil
			}, constant(""))
		})

	type execIn struct {
		Machine   string            `json:"machine" jsonschema:"a machine id or name"`
		Cmd       string            `json:"cmd" jsonschema:"the command line, run through a shell in the guest"`
		Cwd       string            `json:"cwd,omitempty"`
		Env       map[string]string `json:"env,omitempty"`
		User      string            `json:"user,omitempty"`
		TimeoutMS int               `json:"timeout_ms,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "exec", Title: "Run a command",
		Description: "Run a command and wait for it, returning stdout, stderr and the exit code. " +
			"A non-zero exit is a result, not a tool error: decide what it means yourself. " +
			"For output too large to hold in memory, use exec_stream."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Exec(ctx, m.ID, pilots.ExecRequest{Cmd: in.Cmd, Cwd: in.Cwd, Env: in.Env, User: in.User, TimeoutMS: in.TimeoutMS})
			}, constant(""))
		})

	type execStreamIn struct {
		Machine string            `json:"machine" jsonschema:"a machine id or name"`
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		User    string            `json:"user,omitempty"`
		Stdin   bool              `json:"stdin,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "exec_stream", Title: "Run a command over a stream",
		Description: "Run a command over the streaming exec, collecting stdout and stderr. " +
			"stdin is off unless you ask for it: a process holding an open stdin it never reads hangs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in execStreamIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				stream, err := client.Machines.ExecStream(ctx, m.ID, []string{"sh", "-c", in.Cmd}, pilots.ExecStreamOptions{Dir: in.Cwd, Env: in.Env, User: in.User, Stdin: in.Stdin})
				if err != nil {
					return nil, err
				}
				defer stream.Close()
				if in.Stdin && stream.Stdin != nil {
					stream.Stdin.Close()
				}
				stdout, stderr, code, err := stream.Output()
				if err != nil {
					return nil, err
				}
				return map[string]any{"stdout": string(stdout), "stderr": string(stderr), "exit_code": code}, nil
			}, constant(""))
		})

	type logsIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Tail    int    `json:"tail,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "logs", Title: "Console log",
		Description: "The machine's console log, optionally only the last N lines."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				text, err := client.Machines.Logs(ctx, m.ID)
				if err != nil {
					return nil, err
				}
				return map[string]string{"logs": tailLines(text, in.Tail)}, nil
			}, constant(""))
		})

	type checkpointIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Comment string `json:"comment,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "checkpoint", Title: "Checkpoint a machine",
		Description: "Capture the machine, memory included, so it can be restored to this exact moment. " +
			"The resume gap does not grow with the machine size."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in checkpointIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Checkpoint(ctx, m.ID, in.Comment)
			}, func(r any) string { return "restore with checkpoint=" + idOf(r) })
		})

	type restoreIn struct {
		Checkpoint string `json:"checkpoint" jsonschema:"a checkpoint id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "restore", Title: "Restore a checkpoint",
		Description: "Restore a checkpoint IN PLACE. The machine keeps its id, its URL and its agent token; " +
			"nothing new is created, so every link to it still works."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in restoreIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) { return client.Checkpoints.Restore(ctx, in.Checkpoint) }, constant("status on the machine"))
		})

	type buildIn struct {
		Dir        string `json:"dir" jsonschema:"the build context directory"`
		Dockerfile string `json:"dockerfile,omitempty" jsonschema:"Dockerfile CONTENTS (not a path); replaces any Dockerfile in the directory"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "build", Title: "Build a rootfs",
		Description: "Tar a directory and build it into a bootable root filesystem, returning a rootfs build id for deploy. " +
			"Pass `dockerfile` to build with a Dockerfile you wrote here, without writing it to disk first. " +
			"On failure the result carries EVERY log line as NDJSON: read the failing step, fix the Dockerfile, call this again. " +
			dockerfileRules},
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

	type promoteIn struct {
		Machine      string `json:"machine" jsonschema:"a machine id or name"`
		CustomDomain string `json:"custom_domain,omitempty"`
		Replicas     int    `json:"replicas,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "promote", Title: "Promote a machine",
		Description: "Turn a sandbox into a durable service. The URL does not change, which is the whole point: " +
			"every link to the sandbox keeps working against the service."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in promoteIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Promote(ctx, m.ID, pilots.PromoteRequest{CustomDomain: in.CustomDomain, Replicas: in.Replicas})
			}, func(r any) string { return "service with service=" + idOf(r) })
		})

	mcp.AddTool(s, &mcp.Tool{Name: "destroy_machine", Title: "Destroy a machine",
		Description: "Destroy a machine and its snapshots. Irreversible."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in machineIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				if err := client.Machines.Destroy(ctx, m.ID); err != nil {
					return nil, err
				}
				return map[string]string{"destroyed": m.ID}, nil
			}, nil)
		})

	type genIn struct {
		Dir   string `json:"dir"`
		Write bool   `json:"write,omitempty" jsonschema:"also write the Dockerfile, if the directory has none"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "generate_dockerfile", Title: "Generate a Dockerfile",
		Description: "Detect the framework in a directory and return a Dockerfile for it, with the port and health check to deploy it with. " +
			"Use this before `build` on a repo that has no Dockerfile. " + dockerfileRules},
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

	type buildLogsIn struct {
		BuildID string `json:"build_id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "build_logs", Title: "Replay a build log",
		Description: "Replay a build's log by build_id, the id a failed deploy or build named. " +
			"Call it when a result says a build failed and you did not see the lines. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in buildLogsIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				stream, err := client.Builds.Logs(ctx, in.BuildID, false)
				if err != nil {
					return nil, err
				}
				lines := []pilots.BuildLogLine{}
				for line, err := range stream.Lines {
					if err != nil {
						return nil, err
					}
					lines = append(lines, line)
				}
				return map[string]any{"build_id": in.BuildID, "lines": lines}, nil
			}, constant("fix what the line carrying error names, then build again"))
		})

	type noIn struct{}
	mcp.AddTool(s, &mcp.Tool{Name: "list_services", Title: "List services",
		Description: "Every service this key can see, with url, release_id and replicas. " +
			"Call it to find a name before service, releases or logs. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) { return client.Services.List(ctx) }, constant("service with one of these names"))
		})

	type serviceIn struct {
		Service string `json:"service" jsonschema:"a service id or name"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "service", Title: "One service",
		Description: "One service by name or id: its health check, its env KEYS (never the values), its domain, " +
			"its current release and its replica ids. Read-only. Next: releases for history, logs on a replica."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				svc, err := mcpResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				machines, err := client.Machines.List(ctx)
				if err != nil {
					return nil, err
				}
				out, _ := toObject(svc)
				ids := []string{}
				for _, m := range machines {
					if m.ServiceID == svc.ID {
						ids = append(ids, m.ID)
					}
				}
				out["replica_ids"] = ids
				return out, nil
			}, constant("releases for history, or logs on a replica id"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "releases", Title: "A service's releases",
		Description: "A service's releases, newest first, with healthy and the build each came from. " +
			"Call it before rollback. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				svc, err := mcpResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				return client.Services.Releases(ctx, svc.ID)
			}, constant("rollback only if the user agrees to change what is serving"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "rollback", Title: "Roll a service back",
		Description: "Roll a service back to its previous healthy release. " +
			"This CHANGES WHAT IS SERVING: say so and get agreement before calling it. " +
			"Next: service, to confirm release_id moved."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				svc, err := mcpResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				return client.Services.Rollback(ctx, svc.ID)
			}, constant("service, to confirm release_id moved"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "domains", Title: "List custom domains",
		Description: "Every custom domain on this key's services, with whether each is verified. Read-only: " +
			"adding one is `pilot domains add`, because it needs a DNS record the user creates."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) { return client.Domains.List(ctx) }, constant(""))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "volumes", Title: "List volumes",
		Description: "Every volume, with the machine each is attached to. Read-only: a volume is declared in a " +
			"compose file, not created by hand."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) { return client.Volumes.List(ctx) }, constant(""))
		})

	type diagnoseIn struct {
		Replica string `json:"replica" jsonschema:"the replica id from a health_gate_failed error"`
		Tail    int    `json:"tail,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "diagnose", Title: "Diagnose a failed deploy",
		Description: "Explain a failed deploy: the replica's last console lines and what it is doing. " +
			"Call it right after health_gate_failed with details.replica. " +
			"Deterministic, no model. Next: fix the app and deploy again, or rollback."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in diagnoseIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				m, err := resolveMachine(ctx, client, in.Replica)
				if err != nil {
					return nil, err
				}
				text, err := client.Machines.Logs(ctx, m.ID)
				if err != nil {
					return nil, err
				}
				tail := in.Tail
				if tail == 0 {
					tail = 80
				}
				return map[string]any{
					"replica": m.ID, "state": m.State, "url": m.URL, "tail": tailLines(text, tail),
					"checks": []string{
						"is the app listening on 0.0.0.0 rather than 127.0.0.1?",
						"does it read $PORT, with 8080 as the fallback?",
						"did it exit before it bound anything? the last lines say so",
					},
				}, nil
			}, constant("fix the app and deploy again, or rollback"))
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
				m, err := resolveMachine(ctx, client, in.Machine)
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
				m, err := resolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				if err := pullFiles(ctx, client, m.ID, in.Src, in.Dest); err != nil {
					return nil, err
				}
				return map[string]string{"machine": m.ID, "src": in.Src, "dest": in.Dest}, nil
			}, constant(""))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "init", Title: "Read this first",
		Description: "READ THIS FIRST. The pilots mental model in under sixty lines: one primitive, the one-call " +
			"deploy, what every result and error carries, and the doc index. " +
			"Call it once at the start of any pilots task. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				return map[string]any{"primer": primer, "topics": topics(d.skill)}, nil
			}, constant("deploy with dir, once you know the directory"))
		})

	type docsIn struct {
		Topic string `json:"topic,omitempty"`
		Query string `json:"query,omitempty" jsonschema:"search every reference instead of naming one"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "docs", Title: "Read a reference",
		Description: "Read one pilots reference by topic (deploy, sandboxes, services, secrets, volumes, domains, " +
			"promote, errors, compose), or search them with query. No arguments lists the topics. " +
			"Load one. Two at most. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in docsIn) (*mcp.CallToolResult, any, error) {
			return wrap(func() (any, error) {
				if in.Query != "" {
					return map[string]any{"query": in.Query, "matches": searchTopics(d.skill, in.Query)}, nil
				}
				if in.Topic == "" {
					return map[string]any{"topics": topics(d.skill)}, nil
				}
				text, ok := readTopic(d.skill, in.Topic)
				if !ok {
					return nil, fmt.Errorf("no such topic %s; the topics are %s", in.Topic, strings.Join(topics(d.skill), ", "))
				}
				return map[string]any{"topic": in.Topic, "text": text}, nil
			}, constant(""))
		})
}

func (d mcpDeps) registerSkill(s *mcp.Server) {
	for _, page := range skillPages(d.skill) {
		uri := "pilots-docs://" + page.Name
		path := page.Path
		s.AddResource(&mcp.Resource{URI: uri, Name: page.Name, Title: page.Name,
			Description: "The pilots skill: " + page.Name, MIMEType: "text/markdown"},
			func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return nil, err
				}
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: string(raw)}}}, nil
			})
	}
}

func (d mcpDeps) registerPrompts(s *mcp.Server) {
	s.AddPrompt(&mcp.Prompt{Name: "deploy", Title: "Deploy a directory to a URL",
		Description: "Take a directory to a URL in one call, following the platform's next step on any error.",
		Arguments:   []*mcp.PromptArgument{{Name: "dir", Description: "the directory to deploy", Required: true}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			dir := req.Params.Arguments["dir"]
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: fmt.Sprintf(
				"Deploy %s to pilots. Call init first, then deploy with dir=%s. "+
					"On any error, read code, next and details, and do what next says. "+
					"Report the URL when it is serving.", dir, dir)}}}}, nil
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

// mcpResolveService accepts an id or a name, as the TS tools did, and names
// list_services in the refusal so the agent knows where to look.
func mcpResolveService(ctx context.Context, client *pilots.Client, ref string) (*pilots.Service, error) {
	services, err := client.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range services {
		if services[i].ID == ref || services[i].Name == ref {
			return &services[i], nil
		}
	}
	return nil, fmt.Errorf("no service %s; list_services shows what this key can see", ref)
}

func tailLines(text string, n int) string {
	if n <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[len(lines)-n:], "\n")
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
