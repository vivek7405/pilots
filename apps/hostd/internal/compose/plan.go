// Package compose turns a compose file into an ordered plan of pilots
// primitives. The CLI posts the file's text and executes what comes back.
//
// There is ONE compose parser and it is here, in Go, beside the daemon. A
// JavaScript one in the CLI would be a second implementation of a
// specification, and the two would disagree on the day it mattered.
//
// # What the plan is
//
// A Plan is an app name and an ordered list of Steps, one per compose service.
// It is stateless: Plan reads the file's TEXT and an interpolation map, never a
// path and never the host's environment, so the same file plans identically on
// every host in the fleet and on every run.
//
// # Ordering
//
// Kahn's algorithm over depends_on, with the ready set kept sorted by name and
// popped one at a time. compose-go's own graph.InDependencyOrder walks
// concurrently, which makes the resulting order depend on goroutine
// scheduling -- two hosts planning the same file would then build the same app
// in different orders.
//
// # The app name
//
// COMPOSE_PROJECT_NAME in the request's env wins (compose's own override
// order: environment over file), then a top-level x-pilots.app, then a
// top-level name. A file with none of the three is refused rather than given a
// default, because App is what groups the machines that reach each other over
// <name>.internal, and two nameless apps would silently share one namespace.
//
// # x-pilots
//
// Per service: domain, custom_domain, pre_deploy, size_gib (the size of every
// named volume that service declares), and the four replica knobs --
// min_machines_running, auto_stop, auto_start, soft_limit. Top-level: app.
// Unknown keys are tolerated rather than refused, so a compose file written for
// a later CLI still plans here.
//
// # Secrets
//
// An environment value of the form secret://<name> never enters the plan's env.
// It lands in the step's secret_refs as a NAME, the CLI resolves names to
// values from the operator's own store, and hostd seals what comes back.
//
// # What is refused
//
// Every unsupported key in the file is collected and answered in ONE 400, so a
// caller fixes their compose file once rather than N times. The list and its
// messages are in unsupportedKeys.
package compose

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/consts"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// Defaults for a step the file says nothing about. A replica of a service is a
// machine, so these are the machine defaults.
const (
	defaultReplicas  = 1
	defaultVCPUs     = 1
	defaultMemMiB    = 512
	defaultVolumeGiB = 10
	// secretPrefix marks an environment value that names a secret rather than
	// carrying one.
	secretPrefix = "secret://"
	// mib is one mebibyte, the unit compose reports memory limits against.
	mib = 1024 * 1024
)

// Request is what POST /v1/compose/plan accepts.
type Request struct {
	Compose string            `json:"compose"`       // the file's text, not a path
	Env     map[string]string `json:"env,omitempty"` // the interpolation map
}

// Build is a step built from a context directory, relative to the compose file.
type Build struct {
	Context    string `json:"context,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// Volume is a named volume a step mounts.
type Volume struct {
	Name      string `json:"name"`
	SizeGiB   int    `json:"size_gib"`
	MountPath string `json:"mount_path"`
}

// Step is one compose service, as the primitives the CLI will create.
type Step struct {
	Name string `json:"name"`
	// Build is build: {context, dockerfile}, relative to the compose file.
	Build *Build `json:"build,omitempty"`
	// Dockerfile is a generated one, FROM <image> plus whatever the file
	// overrode, for a step that named an image rather than a context. A stock
	// image is a build too: hostd turns any Dockerfile into a bootable rootfs,
	// and a second path for "just pull this" would be a second thing to keep
	// correct.
	//
	// It is also the recipe the detect package generated for a Build context
	// that has no Dockerfile of its own; a step carrying both uses this text
	// as the context's Dockerfile.
	Dockerfile string `json:"dockerfile,omitempty"`
	// DockerfileAppend is what the file overrode on a build: step, rendered as
	// Dockerfile instructions for the CLI to append to the context's own
	// Dockerfile before it uploads it.
	//
	// Dockerfile text rather than the fields themselves, for two reasons. It
	// is the SAME mechanism an image: step already uses, so command:,
	// entrypoint:, working_dir: and user: reach the guest down one path
	// instead of two; and it keeps the CLI free of any opinion about compose
	// or about Docker's override rules, which is what keeps the one parser in
	// this file.
	//
	// Appending is what makes it correct rather than merely convenient. The
	// build reads the FINAL stage of the Dockerfile it is handed
	// (build.ParseStartSpec) and writes what it finds into the image at
	// /etc/pilot-agent/start.json, which the guest agent execs. So an
	// instruction appended here lands in the final stage, overrides what that
	// stage declared, and is delivered by machinery that already exists.
	DockerfileAppend string `json:"dockerfile_append,omitempty"`
	// Env never carries a secret:// value; those are named in SecretRefs.
	Env        map[string]string `json:"env,omitempty"`
	SecretRefs map[string]string `json:"secret_refs,omitempty"` // env key -> secret name
	Ports      []int             `json:"ports,omitempty"`
	Health     *api.HealthCheck  `json:"health,omitempty"`
	Volumes    []Volume          `json:"volumes,omitempty"`
	Replicas   int               `json:"replicas"`
	VCPUs      int               `json:"vcpus"`
	MemMiB     int               `json:"mem_mib"`
	DependsOn  []string          `json:"depends_on,omitempty"`
	// Knobs is the replica lifecycle policy, filled only when the file spelled
	// at least one of the four keys out. The same struct the deploy carries,
	// so the field names both sides write are one declaration.
	Knobs        *api.Knobs `json:"knobs,omitempty"`
	Domain       string     `json:"domain,omitempty"`
	CustomDomain string     `json:"custom_domain,omitempty"`
	PreDeploy    string     `json:"pre_deploy,omitempty"`
}

// Plan is the ordered result: the app, and its services in dependency order.
type Plan struct {
	App   string `json:"app"`
	Steps []Step `json:"steps"`
}

// Unsupported is one key pilots will not honour, and why.
type Unsupported struct {
	Service string `json:"service"`
	Key     string `json:"key"`
	Message string `json:"message"`
}

// PlanError is the 400 body for a file that parses and asks for things this
// platform does not do. Every offending key is listed, so the file is fixed in
// one pass rather than one key per failed deploy.
type PlanError struct {
	Error string `json:"error"`
	// Code and Next mirror api.ErrorResponse so a client branching on the
	// error shape needs one branch, not a second one for this route's body.
	Code        string        `json:"code"`
	Next        string        `json:"next"`
	Unsupported []Unsupported `json:"unsupported"`
}

// unsupportedError is the PlanError's own message, and the string the CLI's
// fixture carries.
const unsupportedError = "compose file has unsupported keys"

// xPilots is the extension block, per service and (for App) at the top level.
//
// mapstructure tags ONLY, and deliberately no json ones. Extensions.Get decodes
// with mapstructure.Decode, so a json tag would do nothing here -- and both
// SDKs' drift tests walk every json-tagged struct in this package and would
// demand a ComposeXPilots mirrored in each of them for a struct that never
// crosses the wire.
//
// Unknown keys decode into nothing and are tolerated: durable_volume, written
// by pilot add postgres, and whatever a later CLI adds.
type xPilots struct {
	Domain             string  `mapstructure:"domain"`
	CustomDomain       string  `mapstructure:"custom_domain"`
	AutoStop           *string `mapstructure:"auto_stop"`
	AutoStart          *bool   `mapstructure:"auto_start"`
	MinMachinesRunning *int    `mapstructure:"min_machines_running"`
	SoftLimit          *int    `mapstructure:"soft_limit"`
	SizeGiB            int     `mapstructure:"size_gib"`
	PreDeploy          string  `mapstructure:"pre_deploy"`
	App                string  `mapstructure:"app"` // top-level only
}

// Compile returns the plan, or a PlanError the caller answers 400 with, or an
// error for a malformed file (also a 400) -- exactly one of the three.
//
// Named Compile rather than Plan because Plan is the type it returns, and the
// SDKs mirror that type by name as ComposePlan.
func Compile(ctx context.Context, req Request) (*Plan, *PlanError, error) {
	var dict map[string]any
	if err := yaml.Unmarshal([]byte(req.Compose), &dict); err != nil {
		return nil, nil, fmt.Errorf("compose: %w", err)
	}
	if dict == nil {
		return nil, nil, errors.New("compose: the file is empty")
	}

	// The pre-scan, BEFORE loading. compose-go reads every env_file from disk
	// while it loads, and a missing one is a load error -- so a check after
	// loading would never run on the server, where the file is not there.
	if bad := envFileKeys(dict); len(bad) > 0 {
		return nil, &PlanError{Error: unsupportedError, Code: api.CodePlanUnsupported,
			Next: "remove or replace each listed key; every one is named", Unsupported: bad}, nil
	}
	if missing := unsetVariables(dict, req.Env); len(missing) > 0 {
		return nil, nil, fmt.Errorf("compose: unset variable %s", strings.Join(missing, ", "))
	}

	app, err := appName(dict, req.Env)
	if err != nil {
		return nil, nil, err
	}

	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(req.Compose)}},
		Environment: req.Env,
	}, func(o *loader.Options) {
		o.SetProjectName(app, true)
		// Nothing on the server has a directory: include: would read files
		// that are not here, and resolving paths would rewrite build.context
		// against a working directory that does not exist. The CLI resolves
		// the context against the compose file's own directory instead.
		o.SkipInclude = true
		o.ResolvePaths = false
		// KnownExtensions is deliberately NOT set. It decodes each x- block
		// through a reflect.New(...).Elem().Interface() target, which is an
		// interface holding a struct VALUE -- mapstructure cannot set fields
		// through one, so every key silently arrives zero. Left unregistered,
		// the block stays the raw map and Extensions.Get decodes it into a
		// real pointer, which is what toStep does.
	})
	if err != nil {
		return nil, nil, fmt.Errorf("compose: %w", err)
	}

	if bad := validate(project.Services); len(bad) > 0 {
		return nil, &PlanError{Error: unsupportedError, Code: api.CodePlanUnsupported,
			Next: "remove or replace each listed key; every one is named", Unsupported: bad}, nil
	}

	steps := make(map[string]Step, len(project.Services))
	for name, svc := range project.Services {
		step, err := toStep(name, svc)
		if err != nil {
			return nil, nil, err
		}
		steps[name] = step
	}
	ordered, err := kahn(steps)
	if err != nil {
		return nil, nil, err
	}
	return &Plan{App: app, Steps: ordered}, nil, nil
}

// envFileKeys collects every service naming an env_file.
func envFileKeys(dict map[string]any) []Unsupported {
	services, _ := dict["services"].(map[string]any)
	var out []Unsupported
	for name := range services {
		svc, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		if _, has := svc["env_file"]; has {
			out = append(out, Unsupported{
				Service: name, Key: "env_file",
				Message: msgEnvFile,
			})
		}
	}
	sortUnsupported(out)
	return out
}

// unsetVariables names every ${VAR} the file gives no way to resolve: no
// modifier, and no entry in the request's env. Sorted.
//
// compose-go's own behaviour is a warning and a blank, and a blank
// DATABASE_URL deployed is worse than a refusal: the app comes up, connects to
// nothing, and the first anyone hears of it is a 500.
//
// What makes a variable optional is the MODIFIER, not the value after it:
// ${TAG:-} says "blank when unset" as deliberately as ${TAG:-latest} says
// "latest when unset", and refusing the first would be refusing a compose file
// the caller has no way to satisfy. template.Variable cannot tell the two
// apart -- DefaultValue is a plain string, so "no default" and "an empty
// default" both arrive as "" -- so optionalVariables re-scans the file's text
// for the modifier itself.
//
// ${VAR:?} and ${VAR?} are deliberately NOT optional here. They say the caller
// must supply a value, which is the refusal this function exists to make.
func unsetVariables(dict map[string]any, env map[string]string) []string {
	optional := map[string]bool{}
	optionalVariables(dict, optional)
	var out []string
	for name, v := range template.ExtractVariables(dict, nil) {
		if v.DefaultValue != "" || v.PresenceValue != "" || optional[name] {
			continue
		}
		if _, ok := env[name]; ok {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// optionalVariables marks every variable written with a - or + modifier, at
// any depth, walking the same three node shapes compose-go's own extractor
// does.
//
// A name written both ways in one file -- ${TAG} in one service, ${TAG:-latest}
// in another -- counts as optional. ExtractVariables already keys by name and
// keeps whichever occurrence it happened to walk last, so the alternative is
// not "refuse it" but "refuse it depending on map iteration order".
func optionalVariables(node any, out map[string]bool) {
	switch v := node.(type) {
	case string:
		scanSubstitutions(v, out)
	case map[string]any:
		for _, elem := range v {
			optionalVariables(elem, out)
		}
	case []any:
		for _, elem := range v {
			optionalVariables(elem, out)
		}
	}
}

// scanSubstitutions marks the optional variables in one string.
func scanSubstitutions(s string, out map[string]bool) {
	for _, match := range template.DefaultPattern.FindAllString(s, -1) {
		scanSubstitution(match, out)
	}
}

// scanSubstitution marks the optional variables in one $-rooted match of
// template.DefaultPattern.
//
// The pattern's braced group is greedy, so "${A:-}${B}" arrives here as ONE
// match rather than two. compose-go answers that by cutting at the first
// BALANCED closing brace and re-scanning the tail, and so does this -- the
// same cut that keeps "${A:-${B}}" from ending at the inner brace.
func scanSubstitution(match string, out map[string]bool) {
	if strings.HasPrefix(match, "$$") { // an escaped $, not a variable
		return
	}
	if !strings.HasPrefix(match, "${") { // $NAME cannot carry a modifier
		return
	}
	end := closingBrace(match)
	if end < 0 {
		return
	}
	body, rest := match[2:end], match[end+1:]
	name, modifier := splitModifier(body)
	switch {
	case name == "":
	case strings.HasPrefix(modifier, ":-"), strings.HasPrefix(modifier, "-"),
		strings.HasPrefix(modifier, ":+"), strings.HasPrefix(modifier, "+"):
		out[name] = true
	}
	// A modifier's own text can hold variables (${A:-${B:-x}}), and so can the
	// tail the greedy match swallowed. Both are strictly shorter than match,
	// so this terminates.
	scanSubstitutions(body, out)
	scanSubstitutions(rest, out)
}

// closingBrace returns the index of the brace closing s's leading "${", or -1.
//
// A transcription of compose-go's getFirstBraceClosingIndex, which is
// unexported. Kept character for character, including the skip of the byte
// after an opening brace, because this decides where a substitution ENDS and a
// version that decided differently would call a variable optional that the
// loader then treats as bare.
func closingBrace(s string) int {
	open := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '}' {
			open--
			if open == 0 {
				return i
			}
		}
		if s[i] == '{' {
			open++
			i++
		}
	}
	return -1
}

// splitModifier cuts a braced body at the first character a variable name
// cannot hold, which is compose-go's own scan in extractVariable. A body that
// starts with one is a name of its own, not an empty name with a modifier.
func splitModifier(body string) (name, modifier string) {
	i := strings.IndexFunc(body, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_':
			return false
		}
		return true
	})
	if i <= 0 {
		return body, ""
	}
	return body[:i], body[i:]
}

// appName resolves the app, in compose's own precedence: environment, then the
// file.
func appName(dict map[string]any, env map[string]string) (string, error) {
	if v := env[consts.ComposeProjectName]; v != "" {
		return v, nil
	}
	var top xPilots
	if raw, ok := dict["x-pilots"]; ok {
		if m, ok := raw.(map[string]any); ok {
			if v, ok := m["app"].(string); ok {
				top.App = v
			}
		}
	}
	if top.App != "" {
		return top.App, nil
	}
	if v, ok := dict["name"].(string); ok && v != "" {
		return v, nil
	}
	return "", errors.New("compose: the compose file has no name: add a " +
		"top-level name: or set COMPOSE_PROJECT_NAME")
}

// unsupportedKeys is every compose key pilots refuses, and the message it
// refuses it with. Collected from what uncloud validates plus what a microVM
// makes moot.
//
// present reports whether the service uses the key at all. fields names the
// types.ServiceConfig fields the rule covers, which is what makes the list
// AUDITABLE rather than merely long: TestEveryServiceKeyIsClassified reflects
// over that struct and fails on a field that is neither honoured, refused, nor
// listed as inert. A compose key that changes behaviour and is silently
// dropped is the failure mode this list exists to make impossible -- it cost a
// deploy of a real application, whose command: and working_dir: both went
// nowhere -- and a compose-go upgrade that adds a key is caught by the same
// test rather than by the next person to be surprised.
var unsupportedKeys = []struct {
	key     string
	fields  []string
	message string
	present func(types.ServiceConfig) bool
}{
	{"security_opt", []string{"SecurityOpt"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.SecurityOpt) > 0 }},
	{"dns", []string{"DNS"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.DNS) > 0 }},
	{"dns_opt", []string{"DNSOpts"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.DNSOpts) > 0 }},
	{"dns_search", []string{"DNSSearch"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.DNSSearch) > 0 }},
	{"labels", []string{"Labels"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Labels) > 0 }},
	{"label_file", []string{"LabelFiles"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.LabelFiles) > 0 }},
	{"links", []string{"Links"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Links) > 0 }},
	{"external_links", []string{"ExternalLinks"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.ExternalLinks) > 0 }},
	{"mem_swappiness", []string{"MemSwappiness"}, msgUnsupported, func(s types.ServiceConfig) bool { return s.MemSwappiness != 0 }},
	{"memswap_limit", []string{"MemSwapLimit"}, msgUnsupported, func(s types.ServiceConfig) bool { return s.MemSwapLimit != 0 }},
	{"mem_reservation", []string{"MemReservation"}, msgUnsupported, func(s types.ServiceConfig) bool { return s.MemReservation != 0 }},
	{"secrets", []string{"Secrets"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Secrets) > 0 }},
	{"configs", []string{"Configs"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Configs) > 0 }},
	{"credential_spec", []string{"CredentialSpec"}, msgUnsupported, func(s types.ServiceConfig) bool { return s.CredentialSpec != nil }},
	{"storage_opt", []string{"StorageOpt"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.StorageOpt) > 0 }},
	{"gpus", []string{"Gpus"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Gpus) > 0 }},
	{"models", []string{"Models"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Models) > 0 }},
	{"provider", []string{"Provider"}, msgUnsupported, func(s types.ServiceConfig) bool { return s.Provider != nil }},
	{"develop", []string{"Develop"}, "develop: drives a local watch loop; there is no local container to sync into",
		func(s types.ServiceConfig) bool { return s.Develop != nil }},
	{"extends", []string{"Extends"}, "extends: reads a file the server does not have; flatten it before deploying",
		func(s types.ServiceConfig) bool { return s.Extends != nil }},
	{"env_file", []string{"EnvFiles"}, msgEnvFile, func(s types.ServiceConfig) bool { return len(s.EnvFiles) > 0 }},
	{"profiles", []string{"Profiles"}, "profiles select which services a local `up` starts; a deploy takes every service in the file",
		func(s types.ServiceConfig) bool { return len(s.Profiles) > 0 }},
	{"container_name", []string{"ContainerName"}, "a replica is a machine and its name is the service's; see x-pilots.domain for the URL",
		func(s types.ServiceConfig) bool { return s.ContainerName != "" }},
	{"hostname", []string{"Hostname"}, msgNaming, func(s types.ServiceConfig) bool { return s.Hostname != "" }},
	{"domainname", []string{"DomainName"}, msgNaming, func(s types.ServiceConfig) bool { return s.DomainName != "" }},
	{"extra_hosts", []string{"ExtraHosts"}, "peers resolve by <name>.internal; there is no hosts file to add to",
		func(s types.ServiceConfig) bool { return len(s.ExtraHosts) > 0 }},
	{"mac_address", []string{"MacAddress"}, "guest addressing is constant so that a snapshot stays host-agnostic",
		func(s types.ServiceConfig) bool { return s.MacAddress != "" }},
	{"net", []string{"Net"}, msgOneNetwork, func(s types.ServiceConfig) bool { return s.Net != "" }},
	{"network_mode", []string{"NetworkMode"}, msgOneNetwork, func(s types.ServiceConfig) bool { return s.NetworkMode != "" }},
	{"logging", []string{"Logging", "LogDriver", "LogOpt"},
		"a machine's output is its journal, read with `pilot logs`",
		func(s types.ServiceConfig) bool {
			return s.Logging != nil || s.LogDriver != "" || len(s.LogOpt) > 0
		}},
	{"volumes_from", []string{"VolumesFrom"}, "a volume is mounted by exactly one machine",
		func(s types.ServiceConfig) bool { return len(s.VolumesFrom) > 0 }},
	{"volume_driver", []string{"VolumeDriver"}, "every volume is JuiceFS over the same bucket",
		func(s types.ServiceConfig) bool { return s.VolumeDriver != "" }},
	{"tmpfs", []string{"Tmpfs"}, msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Tmpfs) > 0 }},
	{"pull_policy", []string{"PullPolicy"}, "every image is built here, and the layer cache decides what is refetched",
		func(s types.ServiceConfig) bool { return s.PullPolicy != "" }},
	{"use_api_socket", []string{"UseAPISocket"}, "there is no docker socket to hand a build",
		func(s types.ServiceConfig) bool { return s.UseAPISocket }},
	{"post_start", []string{"PostStart"}, msgHooks, func(s types.ServiceConfig) bool { return len(s.PostStart) > 0 }},
	{"pre_stop", []string{"PreStop"}, msgHooks, func(s types.ServiceConfig) bool { return len(s.PreStop) > 0 }},
	{"stop_grace_period", []string{"StopGracePeriod"}, msgStop, func(s types.ServiceConfig) bool { return s.StopGracePeriod != nil }},
	{"stop_signal", []string{"StopSignal"}, msgStop, func(s types.ServiceConfig) bool { return s.StopSignal != "" }},

	// Docker's non-swarm resource knobs. `cpus` and `mem_limit` ARE honoured
	// (see vcpusOf and memMiBOf); the rest describe CFS bandwidth and CPU
	// affinity inside a shared kernel, which is not what a machine is given.
	{"cpu_count", []string{"CPUCount"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUCount != 0 }},
	{"cpu_percent", []string{"CPUPercent"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUPercent != 0 }},
	{"cpu_period", []string{"CPUPeriod"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUPeriod != 0 }},
	{"cpu_quota", []string{"CPUQuota"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUQuota != 0 }},
	{"cpu_rt_period", []string{"CPURTPeriod"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPURTPeriod != 0 }},
	{"cpu_rt_runtime", []string{"CPURTRuntime"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPURTRuntime != 0 }},
	{"cpu_shares", []string{"CPUShares"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUShares != 0 }},
	{"cpuset", []string{"CPUSet"}, msgWholeVCPUs, func(s types.ServiceConfig) bool { return s.CPUSet != "" }},

	// The mesh gives every service one flat network and resolves peers by
	// <name>.internal, so the only network a file may name is the default one
	// compose normalisation adds by itself.
	{"networks", []string{"Networks"}, msgOneNetwork, func(s types.ServiceConfig) bool {
		for name := range s.Networks {
			if name != "default" {
				return true
			}
		}
		return len(s.Networks) > 1
	}},
	// The frozen ComposeBuild carries {context, dockerfile} and nothing else.
	{"build.args", []string{}, msgUnsupported, func(s types.ServiceConfig) bool {
		return s.Build != nil && len(s.Build.Args) > 0
	}},
	{"build.target", []string{}, msgUnsupported, func(s types.ServiceConfig) bool {
		return s.Build != nil && s.Build.Target != ""
	}},
	// The legacy v1 spelling of build.dockerfile. compose-go still decodes it,
	// and toStep reads build.dockerfile only -- so honouring one and ignoring
	// the other is exactly the silent drop this list exists to prevent.
	{"dockerfile", []string{"Dockerfile"}, "put it under build.dockerfile",
		func(s types.ServiceConfig) bool { return s.Dockerfile != "" }},

	{"deploy.placement", []string{}, "placement is decided by the fleet", func(s types.ServiceConfig) bool {
		return s.Deploy != nil && (len(s.Deploy.Placement.Constraints) > 0 ||
			len(s.Deploy.Placement.Preferences) > 0 || s.Deploy.Placement.MaxReplicas > 0)
	}},
	{"deploy.mode", []string{}, "a service runs deploy.replicas machines; there is no global mode",
		func(s types.ServiceConfig) bool {
			return s.Deploy != nil && s.Deploy.Mode != "" && s.Deploy.Mode != "replicated"
		}},
	{"deploy.endpoint_mode", []string{}, "the router owns how a service is reached",
		func(s types.ServiceConfig) bool { return s.Deploy != nil && s.Deploy.EndpointMode != "" }},
	{"deploy.labels", []string{}, msgUnsupported,
		func(s types.ServiceConfig) bool { return s.Deploy != nil && len(s.Deploy.Labels) > 0 }},
	{"deploy.update_config", []string{}, msgRollout,
		func(s types.ServiceConfig) bool { return s.Deploy != nil && s.Deploy.UpdateConfig != nil }},
	{"deploy.rollback_config", []string{}, msgRollout,
		func(s types.ServiceConfig) bool { return s.Deploy != nil && s.Deploy.RollbackConfig != nil }},
	{"deploy.restart_policy", []string{}, msgRestart,
		func(s types.ServiceConfig) bool { return s.Deploy != nil && s.Deploy.RestartPolicy != nil }},

	{"privileged", []string{"Privileged"}, msgMoot, func(s types.ServiceConfig) bool { return s.Privileged }},
	{"cap_add", []string{"CapAdd"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.CapAdd) > 0 }},
	{"cap_drop", []string{"CapDrop"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.CapDrop) > 0 }},
	{"userns_mode", []string{"UserNSMode"}, msgMoot, func(s types.ServiceConfig) bool { return s.UserNSMode != "" }},
	{"pid", []string{"Pid"}, msgMoot, func(s types.ServiceConfig) bool { return s.Pid != "" }},
	{"pids_limit", []string{"PidsLimit"}, msgMoot, func(s types.ServiceConfig) bool { return s.PidsLimit != 0 }},
	{"ipc", []string{"Ipc"}, msgMoot, func(s types.ServiceConfig) bool { return s.Ipc != "" }},
	{"uts", []string{"Uts"}, msgMoot, func(s types.ServiceConfig) bool { return s.Uts != "" }},
	{"cgroup", []string{"Cgroup"}, msgMoot, func(s types.ServiceConfig) bool { return s.Cgroup != "" }},
	{"cgroup_parent", []string{"CgroupParent"}, msgMoot, func(s types.ServiceConfig) bool { return s.CgroupParent != "" }},
	{"device_cgroup_rules", []string{"DeviceCgroupRules"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.DeviceCgroupRules) > 0 }},
	{"devices", []string{"Devices"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.Devices) > 0 }},
	{"blkio_config", []string{"BlkioConfig"}, msgMoot, func(s types.ServiceConfig) bool { return s.BlkioConfig != nil }},
	{"init", []string{"Init"}, msgMoot, func(s types.ServiceConfig) bool { return s.Init != nil }},
	{"isolation", []string{"Isolation"}, msgMoot, func(s types.ServiceConfig) bool { return s.Isolation != "" }},
	{"runtime", []string{"Runtime"}, msgMoot, func(s types.ServiceConfig) bool { return s.Runtime != "" }},
	{"oom_kill_disable", []string{"OomKillDisable"}, msgMoot, func(s types.ServiceConfig) bool { return s.OomKillDisable }},
	{"oom_score_adj", []string{"OomScoreAdj"}, msgMoot, func(s types.ServiceConfig) bool { return s.OomScoreAdj != 0 }},
	{"read_only", []string{"ReadOnly"}, msgMoot, func(s types.ServiceConfig) bool { return s.ReadOnly }},
	{"shm_size", []string{"ShmSize"}, msgMoot, func(s types.ServiceConfig) bool { return s.ShmSize != 0 }},
	{"sysctls", []string{"Sysctls"}, "the machine owns its kernel; ship the setting as /etc/sysctl.d in the image",
		func(s types.ServiceConfig) bool { return len(s.Sysctls) > 0 }},
	{"ulimits", []string{"Ulimits"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.Ulimits) > 0 }},
	{"group_add", []string{"GroupAdd"}, msgMoot, func(s types.ServiceConfig) bool { return len(s.GroupAdd) > 0 }},
	{"stdin_open", []string{"StdinOpen"}, msgMoot, func(s types.ServiceConfig) bool { return s.StdinOpen }},
	{"tty", []string{"Tty"}, msgMoot, func(s types.ServiceConfig) bool { return s.Tty }},

	// restart: is honoured wherever it agrees with what a machine does, which
	// is restart-always. `no` is the one value that asks for something else,
	// and a replica that stays dead is exactly what the autoscaler and the
	// health gate are built to prevent.
	{"restart", []string{"Restart"}, "a replica is always restarted; `no` cannot be honoured",
		func(s types.ServiceConfig) bool {
			return s.Restart == "no" || s.Restart == "none"
		}},
	// platform: is honoured when it names what this fleet is.
	{"platform", []string{"Platform"}, "every host is linux/amd64",
		func(s types.ServiceConfig) bool {
			return s.Platform != "" && s.Platform != "linux" && s.Platform != "linux/amd64"
		}},
	// A service's own URL and its health gate both dial GuestAppPort, so a
	// file that publishes ports and none of them is that one describes an
	// application the router can never reach and the rollout can never gate.
	// Other ports beside it are fine: they are reachable at
	// <port>-<name>.<domain>.
	{"ports", []string{"Ports"}, fmt.Sprintf(
		"a service is reached on container port %d; publish it, and reach any "+
			"other port at <port>-<name>.<domain>", netns.GuestAppPort),
		func(s types.ServiceConfig) bool {
			if len(s.Ports) == 0 {
				return false
			}
			for _, p := range s.Ports {
				if int(p.Target) == netns.GuestAppPort {
					return false
				}
			}
			return true
		}},

	{"volumes", []string{"Volumes"}, "bind mounts have no host to bind to; use a named volume",
		func(s types.ServiceConfig) bool {
			for _, v := range s.Volumes {
				if v.Type != types.VolumeTypeVolume {
					return true
				}
			}
			return false
		}},

	{"depends_on.condition", []string{},
		"service_completed_successfully is not supported; use x-pilots.pre_deploy",
		func(s types.ServiceConfig) bool {
			for _, dep := range s.DependsOn {
				if dep.Condition == types.ServiceConditionCompletedSuccessfully {
					return true
				}
			}
			return false
		}},
}

const (
	msgUnsupported = "unsupported in pilots"
	msgMoot        = "moot in a microVM: the service already owns its kernel"
	msgNaming      = "a machine's name is its hostname, and its URL is <name>.<domain>"
	msgOneNetwork  = "every service is on one flat network and resolves peers by <name>.internal"
	msgHooks       = "use x-pilots.pre_deploy, which runs once per deploy rather than once per replica"
	msgStop        = "a replica is suspended rather than signalled; see x-pilots.auto_stop"
	msgRollout     = "the rollout is fixed: boot one, gate it, restore the rest from it, then flip"
	msgRestart     = "a replica is always restarted; see x-pilots.auto_stop for the idle policy"
	msgWholeVCPUs  = "a machine is given whole vCPUs; use deploy.resources.limits.cpus or cpus"
	msgEnvFile     = "env_file has no file to read on the server; put the " +
		"values under environment: or use secret://"
)

// honouredFields is every types.ServiceConfig field the planner reads, and
// inertFields is every field that changes nothing about the process that runs
// -- in Docker either. Both exist for TestEveryServiceKeyIsClassified; see
// unsupportedKeys.
var (
	honouredFields = []string{
		"Name", "Build", "Image", "Command", "Entrypoint", "WorkingDir", "User",
		"Environment", "HealthCheck", "DependsOn", "Deploy", "Scale", "Volumes",
		"Ports", "MemLimit", "CPUS", "Restart", "Platform", "Extensions",
	}
	inertFields = map[string]string{
		"Annotations":  "metadata Docker attaches to a container and nothing reads at run time",
		"Attach":       "whether `docker compose up` streams this service's logs",
		"Expose":       "documentation in Docker too: it opens nothing and publishes nothing",
		"CustomLabels": "compose-go's own bookkeeping; the schema has no such key",
	}
)

// validate collects every unsupported key across every service, sorted by
// service then key, so one 400 carries the whole list.
func validate(services types.Services) []Unsupported {
	var out []Unsupported
	for name, svc := range services {
		for _, rule := range unsupportedKeys {
			if rule.present(svc) {
				out = append(out, Unsupported{Service: name, Key: rule.key, Message: rule.message})
			}
		}
	}
	sortUnsupported(out)
	return out
}

func sortUnsupported(u []Unsupported) {
	sort.Slice(u, func(i, j int) bool {
		if u[i].Service != u[j].Service {
			return u[i].Service < u[j].Service
		}
		return u[i].Key < u[j].Key
	})
}

// toStep maps one loaded service onto the primitives that will run it.
func toStep(name string, svc types.ServiceConfig) (Step, error) {
	var x xPilots
	if _, err := svc.Extensions.Get("x-pilots", &x); err != nil {
		return Step{}, fmt.Errorf("compose: %s: x-pilots: %w", name, err)
	}
	knobs, err := knobsFrom(name, x)
	if err != nil {
		return Step{}, err
	}

	step := Step{
		Name:         name,
		Replicas:     replicasOf(svc),
		VCPUs:        vcpusOf(svc),
		MemMiB:       memMiBOf(svc),
		Ports:        portsOf(svc),
		Volumes:      volumesOf(svc, x.SizeGiB),
		Health:       healthOf(svc),
		DependsOn:    slices.Sorted(maps.Keys(svc.DependsOn)),
		Knobs:        knobs,
		Domain:       x.Domain,
		CustomDomain: x.CustomDomain,
		PreDeploy:    x.PreDeploy,
	}
	step.Env, step.SecretRefs = envOf(svc)

	if svc.Build != nil {
		// build: wins over image:, which in that case is only the tag the
		// result would be pushed under.
		step.Build = &Build{Context: svc.Build.Context, Dockerfile: svc.Build.Dockerfile}
		step.DockerfileAppend = overrides(svc)
	} else if svc.Image != "" {
		step.Dockerfile = "FROM " + svc.Image + "\n" + overrides(svc)
	}
	return step, nil
}

// overrides renders what the file said about how to run the service as the
// Dockerfile instructions that say it.
//
// Instructions rather than a rendered command line, because Docker's own
// override rules are the ones a compose file is written against and only the
// instructions express them: command: replaces the image's CMD and leaves its
// ENTRYPOINT alone -- which is the whole reason a stock postgres that sets only
// command: still runs initdb -- and entrypoint: replaces the ENTRYPOINT and
// CLEARS the image's CMD, so a stale default argument list cannot be appended
// to a program that was never meant to take it.
//
// The empty CMD is written out rather than left implicit for that last reason:
// the build reads the final stage and nothing else, so "the file said nothing"
// and "the file cleared it" have to look different in the text.
func overrides(svc types.ServiceConfig) string {
	var out string
	// WORKDIR first: it is where the command runs, and an instruction that
	// changes the directory belongs before the one that names the program.
	// Appended after everything the context's Dockerfile did, so it moves
	// where the application starts without moving where anything was COPY'd.
	if svc.WorkingDir != "" {
		out += "WORKDIR " + strconv.Quote(svc.WorkingDir) + "\n"
	}
	if svc.User != "" {
		out += "USER " + svc.User + "\n"
	}
	if len(svc.Entrypoint) > 0 {
		out += "ENTRYPOINT " + jsonArray(svc.Entrypoint) + "\n"
		if len(svc.Command) == 0 {
			out += "CMD []\n"
		}
	}
	if len(svc.Command) > 0 {
		out += "CMD " + jsonArray(svc.Command) + "\n"
	}
	return out
}

// jsonArray renders argv as a Dockerfile exec-form array.
func jsonArray(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, strconv.Quote(a))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// envOf splits a service's environment into plain values and secret names.
//
// A nil value is a key that resolved to nothing -- compose's bare `- KEY` form
// with no such variable in the environment -- and is skipped rather than sent
// as an empty string.
func envOf(svc types.ServiceConfig) (map[string]string, map[string]string) {
	var env, refs map[string]string
	for _, key := range slices.Sorted(maps.Keys(svc.Environment)) {
		v := svc.Environment[key]
		if v == nil {
			continue
		}
		if name, ok := strings.CutPrefix(*v, secretPrefix); ok {
			if refs == nil {
				refs = map[string]string{}
			}
			refs[key] = name
			continue
		}
		if env == nil {
			env = map[string]string{}
		}
		env[key] = *v
	}
	return env, refs
}

// portsOf reads the container ports a service listens on. A published host
// port is ignored: the router owns 443 and there is no host to bind to.
func portsOf(svc types.ServiceConfig) []int {
	var out []int
	for _, p := range svc.Ports {
		if p.Target > 0 {
			out = append(out, int(p.Target))
		}
	}
	return out
}

// volumesOf reads the named volumes a service mounts. Bind mounts are refused
// in validate, so anything reaching here is a volume.
func volumesOf(svc types.ServiceConfig, sizeGiB int) []Volume {
	if sizeGiB <= 0 {
		sizeGiB = defaultVolumeGiB
	}
	var out []Volume
	for _, v := range svc.Volumes {
		if v.Type != types.VolumeTypeVolume {
			continue
		}
		out = append(out, Volume{Name: v.Source, SizeGiB: sizeGiB, MountPath: v.Target})
	}
	return out
}

// healthOf maps a compose healthcheck onto the platform's, in whole seconds.
func healthOf(svc types.ServiceConfig) *api.HealthCheck {
	hc := svc.HealthCheck
	if hc == nil {
		return nil
	}
	// Docker's own way of saying "no check", and disable: true means the same.
	if hc.Disable || (len(hc.Test) == 1 && hc.Test[0] == "NONE") {
		return &api.HealthCheck{Type: "none"}
	}
	out := &api.HealthCheck{Type: "cmd", Test: hc.Test}
	out.IntervalSec = seconds(hc.Interval)
	out.TimeoutSec = seconds(hc.Timeout)
	out.GraceSec = seconds(hc.StartPeriod)
	if hc.Retries != nil {
		out.HealthyThreshold = int(*hc.Retries)
	}
	return out
}

// seconds converts a compose duration to whole seconds, rounding up so a
// sub-second interval does not become "never".
func seconds(d *types.Duration) int {
	if d == nil || *d <= 0 {
		return 0
	}
	return int(math.Ceil(time.Duration(*d).Seconds()))
}

// DefaultReplicas, DefaultVCPUs and DefaultMemMiB are what a step gets when
// nothing declared otherwise. Exported so the detect package's generated steps
// agree with a compose file's by construction rather than by coincidence.
const (
	DefaultReplicas = defaultReplicas
	DefaultVCPUs    = defaultVCPUs
	DefaultMemMiB   = defaultMemMiB
)

// replicasOf reads deploy.replicas, then the non-swarm `scale:` that means the
// same thing. Both, because a file that spells only the second one and is read
// for only the first deploys ONE machine and says nothing about it.
func replicasOf(svc types.ServiceConfig) int {
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		return *svc.Deploy.Replicas
	}
	if svc.Scale != nil {
		return *svc.Scale
	}
	return defaultReplicas
}

// vcpusOf rounds a fractional cpu limit UP: a microVM is given whole vCPUs,
// and rounding 1.5 down would quietly halve what the file asked for.
//
// deploy.resources.limits.cpus first, then the non-swarm `cpus:`. Reading only
// the first gives a file that spells only the second a one-vCPU machine and no
// word about why.
func vcpusOf(svc types.ServiceConfig) int {
	cpus := 0.0
	if limits := resourceLimits(svc); limits != nil {
		cpus = float64(limits.NanoCPUs)
	}
	if cpus <= 0 {
		cpus = float64(svc.CPUS)
	}
	if cpus <= 0 {
		return defaultVCPUs
	}
	return int(math.Ceil(cpus))
}

// memMiBOf reads deploy.resources.limits.memory, then the non-swarm
// `mem_limit`, for the reason vcpusOf reads both.
func memMiBOf(svc types.ServiceConfig) int {
	var bytes types.UnitBytes
	if limits := resourceLimits(svc); limits != nil {
		bytes = limits.MemoryBytes
	}
	if bytes <= 0 {
		bytes = svc.MemLimit
	}
	if bytes <= 0 {
		return defaultMemMiB
	}
	// Rounded UP, for the reason vcpusOf rounds up: truncating a limit that is
	// not a whole number of MiB hands the guest less than the file asked for,
	// and truncating one under a mebibyte hands it ZERO -- which the machine
	// layer then silently replaces with its own default.
	return int(math.Ceil(float64(bytes) / mib))
}

// resourceLimits is deploy.resources.limits. Reservations are ignored: a
// reservation is a scheduling floor for a cluster with contention, and a
// microVM is given exactly what it is given.
func resourceLimits(svc types.ServiceConfig) *types.Resource {
	if svc.Deploy == nil {
		return nil
	}
	return svc.Deploy.Resources.Limits
}

// knobsFrom fills the replica policy from x-pilots, and only when the file
// spelled at least one of the four keys out.
//
// Built from the machine defaults rather than from zero, for the reason
// api.DecodeKnobs exists: a struct assembled from zeros would carry
// auto_start: false, and a replica that suspends and then refuses to wake is a
// permanently dead URL earned by setting one unrelated field.
func knobsFrom(name string, x xPilots) (*api.Knobs, error) {
	if x.AutoStop == nil && x.AutoStart == nil && x.MinMachinesRunning == nil && x.SoftLimit == nil {
		return nil, nil
	}
	k := api.DefaultKnobs()
	if x.AutoStop != nil {
		switch *x.AutoStop {
		case "off", "stop", "suspend":
			k.AutoStop = *x.AutoStop
		default:
			return nil, fmt.Errorf("compose: %s: x-pilots.auto_stop is %q, "+
				"want off, stop or suspend", name, *x.AutoStop)
		}
	}
	if x.AutoStart != nil {
		k.AutoStart = *x.AutoStart
	}
	if x.MinMachinesRunning != nil {
		if *x.MinMachinesRunning < 0 {
			return nil, fmt.Errorf("compose: %s: x-pilots.min_machines_running "+
				"cannot be negative", name)
		}
		k.MinMachinesRunning = *x.MinMachinesRunning
	}
	if x.SoftLimit != nil {
		if *x.SoftLimit < 0 {
			return nil, fmt.Errorf("compose: %s: x-pilots.soft_limit cannot be "+
				"negative", name)
		}
		k.SoftLimit = *x.SoftLimit
	}
	return &k, nil
}

// kahn orders the steps so that nothing is built before what it depends on.
//
// The ready set is a sorted slice popped one at a time, so the order is a
// function of the file and of nothing else: the same file plans identically on
// every host and on every run, which is what makes a plan comparable between
// two deploys.
func kahn(steps map[string]Step) ([]Step, error) {
	indegree := map[string]int{}
	dependents := map[string][]string{}
	for name, step := range steps {
		if _, ok := indegree[name]; !ok {
			indegree[name] = 0
		}
		for _, dep := range step.DependsOn {
			if _, ok := steps[dep]; !ok {
				return nil, fmt.Errorf("compose: %s depends on %s, which is not "+
					"a service in this file", name, dep)
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	ready := make([]string, 0, len(steps))
	for name, deg := range indegree {
		if deg == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	out := make([]Step, 0, len(steps))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		out = append(out, steps[name])
		for _, dep := range dependents[name] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
				sort.Strings(ready)
			}
		}
	}

	if len(out) != len(steps) {
		var cycle []string
		for name, deg := range indegree {
			if deg > 0 {
				cycle = append(cycle, name)
			}
		}
		sort.Strings(cycle)
		return nil, fmt.Errorf("compose: dependency cycle among %s",
			strings.Join(cycle, ", "))
	}
	return out, nil
}
