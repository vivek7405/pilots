// Package pilotsmcp is the pilots MCP toolset: the fleet as tools for an
// agent. hostd serves it at /mcp on every host over Streamable HTTP, and
// `pilot mcp` serves it on stdio with the local-filesystem tools added, so a
// tool is defined once and behaves the same wherever an agent reaches it.
//
// Every tool answers JSON with a `next` folded in, so a tool cannot ship
// without saying what to do after it; every failure is isError with the most
// actionable text available -- the server's own body for an API error (it
// already carries code, next and details), every NDJSON line for a failed
// build, the message otherwise.
package pilotsmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vivek7405/pilots/agents"
	pilots "github.com/vivek7405/pilots/sdks/go"
)

// FleetTools are the tools that need only the API. They are what /mcp
// serves, and the first half of what `pilot mcp` serves. Sorted.
var FleetTools = []string{
	"build_logs", "checkpoint", "create_machine", "database", "destroy_machine",
	"diagnose", "docs", "domains", "exec", "exec_stream", "fork", "grant",
	"grants", "init", "list_machines", "list_services", "logs", "metrics",
	"promote", "releases", "restore", "rollback", "service", "status",
	"volumes",
}

// LocalTools are the tools that need the agent's own filesystem: a directory
// to tar, a file to push or pull. Only `pilot mcp` registers them; the hosted
// endpoint has no disk on the agent's side to read. Sorted.
var LocalTools = []string{
	"build", "deploy", "generate_dockerfile", "plan", "pull_file", "push_file",
}

// DockerfileRules is the sentence every tool that produces or consumes a
// Dockerfile carries, because both mistakes produce a build that succeeds and
// a URL that answers 502.
const DockerfileRules = "Any Dockerfile you write must bind 0.0.0.0 (never 127.0.0.1, which serves only the guest itself) " +
	"and read the port from $PORT. Both mistakes produce a build that succeeds and a URL that answers 502."

// Options shape a server.
type Options struct {
	// Version is reported to the client as the server's version.
	Version string
	// Pages is the skill served as pilots-docs:// resources and by the docs
	// tool. Nil means the embedded copy.
	Pages []agents.Page
	// Local says the local-filesystem tools are registered too (by the
	// caller, after NewServer). It changes what `init` tells the agent about
	// deploying: with them, "call deploy with dir"; without, how to get them.
	Local bool
}

// NewServer builds a server with the fleet tools, the skill and the prompts
// registered against one client. The caller adds the local tools when it has
// a filesystem to offer.
func NewServer(client *pilots.Client, opts Options) *mcp.Server {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Pages == nil {
		opts.Pages = agents.Pages()
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "pilots", Version: opts.Version}, nil)
	RegisterFleetTools(s, client, opts)
	RegisterSkill(s, opts.Pages)
	RegisterPrompts(s)
	return s
}

// Wrap shapes a handler's answer. next is a function of the result where
// the next step depends on what came back, and a constant otherwise.
func Wrap(fn func() (any, error), next func(any) string) (*mcp.CallToolResult, any, error) {
	result, err := fn()
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: ErrorText(err)}}}, nil, nil
	}
	step := ""
	if next != nil {
		step = next(result)
	}
	var body any
	if m, ok := ToObject(result); ok {
		m["next"] = step
		body = m
	} else {
		body = map[string]any{"result": result, "next": step}
	}
	text, _ := json.MarshalIndent(body, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(text)}}}, nil, nil
}

// Constant is a next step that does not depend on the result.
func Constant(next string) func(any) string { return func(any) string { return next } }

// ToObject round-trips a struct through JSON so `next` can be added beside
// its fields rather than wrapping it.
func ToObject(v any) (map[string]any, bool) {
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

// ErrorText is the most actionable text an error carries: every NDJSON line
// of a failed build, hostd's own body for an API refusal, the message else.
func ErrorText(err error) string {
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
		// The body verbatim: it already carries error, code, next and
		// details, in the shape every other client reads.
		if strings.HasPrefix(strings.TrimSpace(apiErr.Body), "{") {
			return apiErr.Body
		}
		if raw, e := json.Marshal(apiErr); e == nil {
			return string(raw)
		}
	}
	return err.Error()
}

// IDOf reads an `id` out of a result, for a next step that names it.
func IDOf(v any) string {
	if m, ok := ToObject(v); ok {
		if id, ok := m["id"].(string); ok {
			return id
		}
	}
	return ""
}

// ResolveMachine accepts an id or a name, and names list_machines in the
// refusal so the agent knows where to look.
func ResolveMachine(ctx context.Context, client *pilots.Client, idOrName string) (*pilots.Machine, error) {
	m, err := client.Machines.Get(ctx, idOrName)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, pilots.ErrNotFound) {
		return nil, err
	}
	all, err := client.Machines.List(ctx)
	if err != nil {
		return nil, err
	}
	var named []pilots.Machine
	for _, m := range all {
		if m.Name == idOrName {
			named = append(named, m)
		}
	}
	switch len(named) {
	case 1:
		return &named[0], nil
	case 0:
		return nil, fmt.Errorf("no machine with id or name %s; list_machines shows what this key can see", idOrName)
	}
	ids := make([]string, len(named))
	for i, m := range named {
		ids[i] = m.ID
	}
	return nil, fmt.Errorf("%d machines are named %s: %s; use one of the ids", len(named), idOrName, strings.Join(ids, ", "))
}

// ResolveService is the same rule for services.
func ResolveService(ctx context.Context, client *pilots.Client, ref string) (*pilots.Service, error) {
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

// TailLines keeps the last n lines of text; n <= 0 keeps all of it.
func TailLines(text string, n int) string {
	if n <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// MachineIn is the one-argument input most machine tools take.
type MachineIn struct {
	Machine string `json:"machine" jsonschema:"a machine id or name"`
}

// RegisterFleetTools registers every tool in FleetTools against client.
func RegisterFleetTools(s *mcp.Server, client *pilots.Client, opts Options) {
	pages := opts.Pages
	if pages == nil {
		pages = agents.Pages()
	}

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
		Labels     map[string]string `json:"labels,omitempty" jsonschema:"labels to find it by later; list_machines filters on them"`

		IdleTimeout int               `json:"idle_timeout,omitempty" jsonschema:"seconds of quiet before the machine suspends, 1..3600 (default 60); set it for a daemon nothing connects to"`
		Schedules   []pilots.Schedule `json:"schedules,omitempty" jsonschema:"cron jobs: each is {cron, path} to GET a path on the machine on that schedule (five fields, UTC, or @hourly/@daily/@weekly/@monthly), or {cron, cmd} to run a command in it; the machine is woken for it"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "create_machine", Title: "Create a machine",
		Description: "Create a microVM. A create is a restore from a template rather than a boot, so it is fast. " +
			"The same primitive serves both a throwaway sandbox and a production replica; only the lifecycle knobs differ. " +
			"It suspends after idle_timeout seconds of quiet (default 60) and wakes on the next request or exec; a console session running a command counts as activity."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				req := pilots.CreateMachineRequest{
					Name: in.Name, Image: in.Image, Template: in.Template, Checkpoint: in.Checkpoint,
					VCPUs: in.VCPUs, MemMiB: in.MemMiB, App: in.App, Cmd: in.Cmd, Env: in.Env, Labels: in.Labels,
				}
				if in.IdleTimeout != 0 || len(in.Schedules) > 0 {
					req.Knobs = &pilots.KnobsPatch{}
					if in.IdleTimeout != 0 {
						req.Knobs.IdleTimeout = pilots.Ptr(in.IdleTimeout)
					}
					if len(in.Schedules) > 0 {
						req.Knobs.Schedules = &in.Schedules
					}
				}
				return client.Machines.Create(ctx, req)
			}, Constant("exec on the returned id"))
		})

	type listIn struct {
		App    string            `json:"app,omitempty"`
		Labels map[string]string `json:"labels,omitempty" jsonschema:"only machines carrying every one of these labels"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "list_machines", Title: "List machines",
		Description: "Every machine this API key can see, optionally narrowed to one app or to machines carrying given labels."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				all, err := client.Machines.List(ctx)
				if err != nil || (in.App == "" && len(in.Labels) == 0) {
					return all, err
				}
				kept := []pilots.Machine{}
				for _, m := range all {
					if (in.App == "" || m.App == in.App) && hasLabels(m.Labels, in.Labels) {
						kept = append(kept, m)
					}
				}
				return kept, nil
			}, Constant(""))
		})

	type statusIn struct {
		Machine string `json:"machine,omitempty" jsonschema:"a machine id or name"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "status", Title: "Status",
		Description: "With a machine, that machine. Without one, the fleet: every host as the answering host sees it, " +
			"plus a count of machines by state."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in statusIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				if in.Machine != "" {
					return ResolveMachine(ctx, client, in.Machine)
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
			}, Constant(""))
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
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Exec(ctx, m.ID, pilots.ExecRequest{Cmd: in.Cmd, Cwd: in.Cwd, Env: in.Env, User: in.User, TimeoutMS: in.TimeoutMS})
			}, Constant(""))
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
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
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
			}, Constant(""))
		})

	type logsIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Tail    int    `json:"tail,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "logs", Title: "Console log",
		Description: "The machine's console log, optionally only the last N lines."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				text, err := client.Machines.Logs(ctx, m.ID)
				if err != nil {
					return nil, err
				}
				return map[string]string{"logs": TailLines(text, in.Tail)}, nil
			}, Constant(""))
		})

	type checkpointIn struct {
		Machine string `json:"machine" jsonschema:"a machine id or name"`
		Comment string `json:"comment,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "checkpoint", Title: "Checkpoint a machine",
		Description: "Capture the machine, memory included, so it can be restored to this exact moment. " +
			"The resume gap does not grow with the machine size."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in checkpointIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Checkpoint(ctx, m.ID, in.Comment)
			}, func(r any) string { return "restore with checkpoint=" + IDOf(r) })
		})

	type restoreIn struct {
		Checkpoint string `json:"checkpoint" jsonschema:"a checkpoint id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "restore", Title: "Restore a checkpoint",
		Description: "Restore a checkpoint IN PLACE. The machine keeps its id, its URL and its agent token; " +
			"nothing new is created, so every link to it still works."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in restoreIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) { return client.Checkpoints.Restore(ctx, in.Checkpoint) }, Constant("status on the machine"))
		})

	type forkIn struct {
		Source string `json:"source" jsonschema:"a machine id or name, or a checkpoint id"`
		Count  int    `json:"count,omitempty" jsonschema:"how many forks, default 1, up to 100"`
		Name   string `json:"name,omitempty" jsonschema:"name the first fork; the rest take a suffix"`
		Volume bool   `json:"volume,omitempty" jsonschema:"fork the source's volume too"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "fork", Title: "Fork a machine",
		Description: "Make NEW machines from a machine's or checkpoint's exact state: the source's processes " +
			"already running, its memory already warm. Use this when getting to a state is the expensive part " +
			"-- installing dependencies, loading a model, reaching a reproduction -- and you want several " +
			"machines that all start from there. A running source is checkpointed in place and keeps its id " +
			"and URL; a suspended source is forked without being woken. Each fork is a separate machine with " +
			"its own id and URL."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in forkIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				req := pilots.ForkRequest{Name: in.Name, Count: in.Count, Volume: in.Volume}
				// A machine NAME wins over a checkpoint id, which is what an
				// agent that typed a name expects. A source that resolves to no
				// machine is tried as a checkpoint.
				if m, err := ResolveMachine(ctx, client, in.Source); err == nil {
					return client.Machines.Fork(ctx, m.ID, req)
				}
				return client.Checkpoints.Fork(ctx, in.Source, req)
			}, Constant("exec on any fork; each has its own id and URL"))
		})

	type databaseIn struct {
		Service string `json:"service,omitempty" jsonschema:"a service id or name; omit when there is only one database"`
	}
	// The tool an agent needs before it can touch a database, and NOT a
	// credential.
	//
	// It answers where the database is, what engine it runs and which address
	// does what, and it deliberately answers none of "what is the password".
	// The password is not in the fleet's reach: it lives in the credentials
	// file on the operator's own machine, and an MCP server that could hand one
	// back would turn every API key into a database password.
	//
	// So the next step is a command the PERSON runs. That is the honest shape
	// for this one: an agent that cannot read the data cannot leak it, and an
	// operator who wants it read out loud can run one line.
	mcp.AddTool(s, &mcp.Tool{Name: "database", Title: "Find a database",
		Description: "Where a database is, what engine it runs, and which address to use for what. " +
			"Returns the .internal addresses -- the pooled one an application should use and the direct " +
			"one migrations and anything using LISTEN/NOTIFY, session advisory locks or temporary tables " +
			"must use -- plus the machine to exec in. It does NOT return a password: passwords live on the " +
			"operator's own machine and never in the fleet. To open a session, tell the operator to run " +
			"`pilot db connect <service>`."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in databaseIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) { return describeDatabase(ctx, client, in.Service) },
				Constant("exec on the machine, or tell the operator to run `pilot db connect`"))
		})

	// What a machine is USING, as opposed to what it was allotted.
	//
	// The tool for "why is this slow" and "why did this die": the allotment is
	// already on the machine, and the difference between the two is the answer.
	mcp.AddTool(s, &mcp.Tool{Name: "metrics", Title: "What a machine is using",
		Description: "CPU seconds used and memory held now, against the ceilings, read from the machine s cgroup " +
			"on the host that owns it. CPU is a TOTAL, not a rate: take two readings to get a rate. Memory is " +
			"zero while a machine is suspended, which is the truth rather than a gap. Memory near the ceiling " +
			"is why a process was killed; CPU flat while a request hangs means it is waiting, not computing."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in MachineIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Metrics(ctx, m.ID)
			}, Constant("logs for what it printed, or exec to look inside"))
		})

	type grantIn struct {
		Machine string            `json:"machine,omitempty" jsonschema:"a machine id or name; give this or service"`
		Service string            `json:"service,omitempty" jsonschema:"a service id or name; every replica inherits it"`
		Scopes  []string          `json:"scopes,omitempty" jsonschema:"scopes a token may carry: machines, deploy. Never admin"`
		Secrets map[string]string `json:"secrets,omitempty" jsonschema:"NAME to value the machine may fetch from its broker"`
	}
	// Giving a machine the right to act for itself.
	//
	// The tool an agent reaches for when the thing it built has to call the API
	// or hold a credential of its own. What it grants is deliberately narrow: a
	// machine's token writes only to that machine and to its own service, and
	// `admin` cannot be granted at all.
	//
	// REPLACES. Calling it with only scopes removes every granted secret, and
	// the description says so, because an agent that expected a merge would
	// silently take a secret away from a running application.
	mcp.AddTool(s, &mcp.Tool{Name: "grant", Title: "Grant a machine its credentials",
		Description: "Let a machine ask its host for an API token, for secrets, or both. A machine holds no key " +
			"otherwise, which is deliberate: a key baked into a guest is a key in every snapshot and fork of it. " +
			"Granted secrets never enter the machine's environment, so they are in no snapshot and on no disk " +
			"inside it. This REPLACES the whole grant: pass everything you want it to have, because omitting a " +
			"field removes what was there. You can grant only scopes your own key holds, and never admin. A " +
			"machine's token may write only to that machine and its own service."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in grantIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				req := pilots.GrantRequest{Scopes: in.Scopes, Secrets: in.Secrets}
				if in.Service != "" {
					svc, err := ResolveService(ctx, client, in.Service)
					if err != nil {
						return nil, err
					}
					return client.Services.Grant(ctx, svc.ID, req)
				}
				if in.Machine == "" {
					return nil, fmt.Errorf("name a machine or a service to grant")
				}
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Grant(ctx, m.ID, req)
			}, Constant("the machine picks it up within five minutes; exec `cat $PILOT_TOKEN_FILE` to see it arrive"))
		})

	type grantsIn struct {
		Machine string `json:"machine,omitempty" jsonschema:"a machine id or name"`
		Service string `json:"service,omitempty" jsonschema:"a service id or name"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "grants", Title: "What a machine may ask for",
		Description: "The scopes and the secret NAMES granted to a machine or a service. Never the values: there " +
			"is no route that returns one. What a machine is holding is a question its own broker answers, to it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in grantsIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				if in.Service != "" {
					svc, err := ResolveService(ctx, client, in.Service)
					if err != nil {
						return nil, err
					}
					return client.Services.GrantOf(ctx, svc.ID)
				}
				if in.Machine == "" {
					return nil, fmt.Errorf("name a machine or a service")
				}
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.GrantOf(ctx, m.ID)
			}, Constant("grant to change it"))
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
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				return client.Machines.Promote(ctx, m.ID, pilots.PromoteRequest{CustomDomain: in.CustomDomain, Replicas: in.Replicas})
			}, func(r any) string { return "service with service=" + IDOf(r) })
		})

	mcp.AddTool(s, &mcp.Tool{Name: "destroy_machine", Title: "Destroy a machine",
		Description: "Destroy a machine and its snapshots. Irreversible."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in MachineIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Machine)
				if err != nil {
					return nil, err
				}
				if err := client.Machines.Destroy(ctx, m.ID); err != nil {
					return nil, err
				}
				return map[string]string{"destroyed": m.ID}, nil
			}, nil)
		})

	type buildLogsIn struct {
		BuildID string `json:"build_id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "build_logs", Title: "Replay a build log",
		Description: "Replay a build's log by build_id, the id a failed deploy or build named. " +
			"Call it when a result says a build failed and you did not see the lines. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in buildLogsIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
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
			}, Constant("fix what the line carrying error names, then build again"))
		})

	type noIn struct{}
	mcp.AddTool(s, &mcp.Tool{Name: "list_services", Title: "List services",
		Description: "Every service this key can see, with url, release_id and replicas. " +
			"Call it to find a name before service, releases or logs. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) { return client.Services.List(ctx) }, Constant("service with one of these names"))
		})

	type serviceIn struct {
		Service string `json:"service" jsonschema:"a service id or name"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "service", Title: "One service",
		Description: "One service by name or id: its health check, its env KEYS (never the values), its domain, " +
			"its current release and its replica ids. Read-only. Next: releases for history, logs on a replica."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				svc, err := ResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				machines, err := client.Machines.List(ctx)
				if err != nil {
					return nil, err
				}
				out, _ := ToObject(svc)
				ids := []string{}
				for _, m := range machines {
					if m.ServiceID == svc.ID {
						ids = append(ids, m.ID)
					}
				}
				out["replica_ids"] = ids
				return out, nil
			}, Constant("releases for history, or logs on a replica id"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "releases", Title: "A service's releases",
		Description: "A service's releases, newest first, with healthy and the build each came from. " +
			"Call it before rollback. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				svc, err := ResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				return client.Services.Releases(ctx, svc.ID)
			}, Constant("rollback only if the user agrees to change what is serving"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "rollback", Title: "Roll a service back",
		Description: "Roll a service back to its previous healthy release. " +
			"This CHANGES WHAT IS SERVING: say so and get agreement before calling it. " +
			"Next: service, to confirm release_id moved."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in serviceIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				svc, err := ResolveService(ctx, client, in.Service)
				if err != nil {
					return nil, err
				}
				return client.Services.Rollback(ctx, svc.ID)
			}, Constant("service, to confirm release_id moved"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "domains", Title: "List custom domains",
		Description: "Every custom domain on this key's services, with whether each is verified. Read-only: " +
			"adding one is `pilot domains add`, because it needs a DNS record the user creates."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) { return client.Domains.List(ctx) }, Constant(""))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "volumes", Title: "List volumes",
		Description: "Every volume, with the machine each is attached to. Read-only: a volume is declared in a " +
			"compose file, not created by hand."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) { return client.Volumes.List(ctx) }, Constant(""))
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
			return Wrap(func() (any, error) {
				m, err := ResolveMachine(ctx, client, in.Replica)
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
					"replica": m.ID, "state": m.State, "url": m.URL, "tail": TailLines(text, tail),
					"checks": []string{
						"is the app listening on 0.0.0.0 rather than 127.0.0.1?",
						"does it read $PORT, with 8080 as the fallback?",
						"did it exit before it bound anything? the last lines say so",
					},
				}, nil
			}, Constant("fix the app and deploy again, or rollback"))
		})

	mcp.AddTool(s, &mcp.Tool{Name: "init", Title: "Read this first",
		Description: "READ THIS FIRST. The pilots mental model in under sixty lines: one primitive, the one-call " +
			"deploy, what every result and error carries, and the doc index. " +
			"Call it once at the start of any pilots task. Read-only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noIn) (*mcp.CallToolResult, any, error) {
			return Wrap(func() (any, error) {
				out := map[string]any{"primer": Primer, "topics": Topics(pages)}
				if !opts.Local {
					out["local_tools"] = "This is the hosted server, so " + strings.Join(LocalTools, ", ") +
						" are not here: they read the agent's own filesystem. For those, run `pilot mcp` on stdio " +
						"(pilot mcp install <harness> --stdio), or use `pilot deploy` in a shell. " +
						"Everything else -- machines, exec, checkpoints, services, releases, rollback -- is here."
				}
				return out, nil
			}, Constant("deploy with dir, once you know the directory"))
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
			return Wrap(func() (any, error) {
				if in.Query != "" {
					return map[string]any{"query": in.Query, "matches": SearchTopics(pages, in.Query)}, nil
				}
				if in.Topic == "" {
					return map[string]any{"topics": Topics(pages)}, nil
				}
				text, ok := ReadTopic(pages, in.Topic)
				if !ok {
					return nil, fmt.Errorf("no such topic %s; the topics are %s", in.Topic, strings.Join(Topics(pages), ", "))
				}
				return map[string]any{"topic": in.Topic, "text": text}, nil
			}, Constant(""))
		})
}

// RegisterSkill serves the pages as pilots-docs:// resources.
func RegisterSkill(s *mcp.Server, pages []agents.Page) {
	for _, page := range pages {
		uri := "pilots-docs://" + page.Name
		body := page.Body
		s.AddResource(&mcp.Resource{URI: uri, Name: page.Name, Title: page.Name,
			Description: "The pilots skill: " + page.Name, MIMEType: "text/markdown"},
			func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: body}}}, nil
			})
	}
}

// RegisterPrompts registers the `deploy` prompt.
func RegisterPrompts(s *mcp.Server) {
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

// Topics is what `docs` accepts, derived from the pages rather than listed
// twice. Never nil: an agent reading it gets a list, not JSON null.
func Topics(pages []agents.Page) []string {
	out := []string{}
	for _, p := range pages {
		if strings.HasPrefix(p.Name, "references/") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(p.Name, "references/"), ".md"))
		}
	}
	sort.Strings(out)
	return out
}

// ReadTopic is one reference page's text.
func ReadTopic(pages []agents.Page, topic string) (string, bool) {
	for _, p := range pages {
		if p.Name == "references/"+topic+".md" {
			return p.Body, true
		}
	}
	return "", false
}

// TopicMatch is one hit of SearchTopics.
type TopicMatch struct {
	Topic   string `json:"topic"`
	Excerpt string `json:"excerpt"`
}

// SearchTopics is a case-insensitive substring search with one excerpt per
// page: enough to pick a page, not a search engine.
func SearchTopics(pages []agents.Page, query string) []TopicMatch {
	q := strings.ToLower(query)
	out := []TopicMatch{}
	for _, p := range pages {
		if !strings.HasPrefix(p.Name, "references/") {
			continue
		}
		i := strings.Index(strings.ToLower(p.Body), q)
		if i < 0 {
			continue
		}
		start, end := max(0, i-80), min(len(p.Body), i+len(q)+80)
		out = append(out, TopicMatch{
			Topic:   strings.TrimSuffix(strings.TrimPrefix(p.Name, "references/"), ".md"),
			Excerpt: strings.TrimSpace(p.Body[start:end]),
		})
	}
	return out
}

// Primer is the `init` tool's answer. Under sixty lines on purpose: it is the
// first thing a small model reads, it competes for the same context as the
// task, and a primer nobody finishes is worse than none. A test holds it to
// the budget, because the natural drift is upward.
const Primer = `pilots: sandboxes and services on one primitive.

THE ONE CALL
  deploy { "dir": "<absolute path>" }
  -> { app, services: [{ name, url, release_id }], next }
  The host decides what the directory is: a compose file, a Dockerfile,
  a recipe (webjs, next, react-router, remix, vite, django, fastapi, rails, go,
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
  the first into the second without changing its URL.

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

// DatabaseInfo is what an agent needs to reason about a database, and nothing
// it could leak.
type DatabaseInfo struct {
	Service string `json:"service"`
	Engine  string `json:"engine"`
	Machine string `json:"machine,omitempty"`
	// Address is where the application should connect, and Direct is where
	// migrations and any session-level feature must connect. They differ only
	// when the database has a pooler in front of it.
	Address string `json:"address"`
	Direct  string `json:"direct,omitempty"`
	// Note says, in one line, why there are two of them.
	Note string `json:"note,omitempty"`
}

// describeDatabase finds one database and says how to reach it.
//
// With no name it answers only when there is exactly one, and otherwise lists
// what it found. Picking one of several because it sorts first is how an agent
// ends up running a migration against the wrong database.
func describeDatabase(ctx context.Context, client *pilots.Client, name string) (any, error) {
	services, err := client.Services.List(ctx)
	if err != nil {
		return nil, err
	}
	var found *pilots.Service
	var databases []string
	for i := range services {
		svc := &services[i]
		if svc.Labels["pilot.engine"] == "" {
			continue
		}
		databases = append(databases, svc.Name)
		if name == "" || svc.Name == name || svc.ID == name {
			if found == nil || svc.Name == name || svc.ID == name {
				found = svc
			}
		}
	}
	if len(databases) == 0 {
		return nil, fmt.Errorf("no database in this org; `pilot add postgres` adds one")
	}
	if name == "" && len(databases) > 1 {
		sort.Strings(databases)
		return nil, fmt.Errorf("there are %d databases here (%s); name one",
			len(databases), strings.Join(databases, ", "))
	}
	if found == nil {
		return nil, fmt.Errorf("no database called %q; there is %s",
			name, strings.Join(databases, ", "))
	}

	info := DatabaseInfo{Service: found.Name, Engine: found.Labels["pilot.engine"]}
	port := map[string]string{
		"postgres": "5432", "mysql": "3306", "redis": "6379", "mongo": "27017",
	}[info.Engine]
	info.Address = found.Name + ".internal:" + port
	if info.Engine == "postgres" {
		// The pooled address is the one an application should hold, and it is
		// only there when a pooler was added. Saying so beats guessing: a
		// connection to 6432 with nothing behind it fails in a way that reads
		// as the database being down.
		info.Direct = info.Address
		info.Address = found.Name + ".internal:6432"
		info.Note = "6432 is the pooler, if this database has one; 5432 is direct. " +
			"Migrations, LISTEN/NOTIFY, session advisory locks and temporary tables " +
			"need the direct address."
	}
	machines, err := client.Machines.List(ctx)
	if err == nil {
		for i := range machines {
			if machines[i].ServiceID == found.ID && machines[i].State == "running" {
				info.Machine = machines[i].ID
				break
			}
		}
	}
	return info, nil
}
