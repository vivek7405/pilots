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
	// Dockerfile is a generated one, FROM <image> plus any ENTRYPOINT and CMD
	// the file set, for a step that named an image rather than a context. A
	// stock image is a build too: hostd turns any Dockerfile into a bootable
	// rootfs, and a second path for "just pull this" would be a second thing
	// to keep correct.
	Dockerfile string `json:"dockerfile,omitempty"`
	// Cmd is command:/entrypoint: on a build: step, shell-quoted per element.
	Cmd string `json:"cmd,omitempty"`
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
	Error       string        `json:"error"`
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
		return nil, &PlanError{Error: unsupportedError, Unsupported: bad}, nil
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
		return nil, &PlanError{Error: unsupportedError, Unsupported: bad}, nil
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
				Message: "env_file has no file to read on the server; put the " +
					"values under environment: or use secret://",
			})
		}
	}
	sortUnsupported(out)
	return out
}

// unsetVariables names every ${VAR} with no default, no presence value and no
// entry in the request's env, sorted.
//
// compose-go's own behaviour is a warning and a blank, and a blank
// DATABASE_URL deployed is worse than a refusal: the app comes up, connects to
// nothing, and the first anyone hears of it is a 500.
func unsetVariables(dict map[string]any, env map[string]string) []string {
	var out []string
	for name, v := range template.ExtractVariables(dict, nil) {
		if v.DefaultValue != "" || v.PresenceValue != "" {
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
// present reports whether the service uses the key at all.
var unsupportedKeys = []struct {
	key     string
	message string
	present func(types.ServiceConfig) bool
}{
	{"security_opt", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.SecurityOpt) > 0 }},
	{"dns", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.DNS) > 0 }},
	{"dns_search", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.DNSSearch) > 0 }},
	{"labels", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Labels) > 0 }},
	{"links", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Links) > 0 }},
	{"mem_swappiness", msgUnsupported, func(s types.ServiceConfig) bool { return s.MemSwappiness != 0 }},
	{"memswap_limit", msgUnsupported, func(s types.ServiceConfig) bool { return s.MemSwapLimit != 0 }},
	{"secrets", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.Secrets) > 0 }},
	{"storage_opt", msgUnsupported, func(s types.ServiceConfig) bool { return len(s.StorageOpt) > 0 }},
	// The mesh gives every service one flat network and resolves peers by
	// <name>.internal, so the only network a file may name is the default one
	// compose normalisation adds by itself.
	{"networks", msgUnsupported, func(s types.ServiceConfig) bool {
		for name := range s.Networks {
			if name != "default" {
				return true
			}
		}
		return len(s.Networks) > 1
	}},
	// The frozen ComposeBuild carries {context, dockerfile} and nothing else.
	{"build.args", msgUnsupported, func(s types.ServiceConfig) bool {
		return s.Build != nil && len(s.Build.Args) > 0
	}},
	{"build.target", msgUnsupported, func(s types.ServiceConfig) bool {
		return s.Build != nil && s.Build.Target != ""
	}},

	{"deploy.placement", "placement is decided by the fleet", func(s types.ServiceConfig) bool {
		return s.Deploy != nil && (len(s.Deploy.Placement.Constraints) > 0 ||
			len(s.Deploy.Placement.Preferences) > 0 || s.Deploy.Placement.MaxReplicas > 0)
	}},

	{"privileged", msgMoot, func(s types.ServiceConfig) bool { return s.Privileged }},
	{"cap_add", msgMoot, func(s types.ServiceConfig) bool { return len(s.CapAdd) > 0 }},
	{"cap_drop", msgMoot, func(s types.ServiceConfig) bool { return len(s.CapDrop) > 0 }},
	{"userns_mode", msgMoot, func(s types.ServiceConfig) bool { return s.UserNSMode != "" }},
	{"pid", msgMoot, func(s types.ServiceConfig) bool { return s.Pid != "" }},
	{"devices", msgMoot, func(s types.ServiceConfig) bool { return len(s.Devices) > 0 }},

	{"volumes", "bind mounts have no host to bind to; use a named volume",
		func(s types.ServiceConfig) bool {
			for _, v := range s.Volumes {
				if v.Type != types.VolumeTypeVolume {
					return true
				}
			}
			return false
		}},

	{"depends_on.condition",
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
		step.Cmd = shellCommand(svc.Entrypoint, svc.Command)
	} else if svc.Image != "" {
		step.Dockerfile = imageDockerfile(svc)
	}
	return step, nil
}

// imageDockerfile renders a stock image as the one-line Dockerfile that builds
// it, plus whatever the file overrode.
//
// The pair goes in as JSON-array ENTRYPOINT/CMD lines rather than as the step's
// cmd, so a stock postgres image that sets only command: keeps its OWN
// entrypoint -- and with it initdb, which is the whole reason that image works
// without one.
func imageDockerfile(svc types.ServiceConfig) string {
	out := "FROM " + svc.Image + "\n"
	if len(svc.Entrypoint) > 0 {
		out += "ENTRYPOINT " + jsonArray(svc.Entrypoint) + "\n"
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

// shellCommand joins entrypoint and command into one shell line, quoting each
// element.
//
// Per ELEMENT, not per word: compose's list form already decided where the
// argument boundaries are, and re-splitting on spaces would turn
// archive_command=test ! -f x && cp y x into five arguments and a shell
// operator the guest would then run.
func shellCommand(entrypoint, command []string) string {
	argv := append(append([]string{}, entrypoint...), command...)
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps one argument in POSIX single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

func replicasOf(svc types.ServiceConfig) int {
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		return *svc.Deploy.Replicas
	}
	return defaultReplicas
}

// vcpusOf rounds a fractional cpu limit UP: a microVM is given whole vCPUs,
// and rounding 1.5 down would quietly halve what the file asked for.
func vcpusOf(svc types.ServiceConfig) int {
	limits := resourceLimits(svc)
	if limits == nil || limits.NanoCPUs <= 0 {
		return defaultVCPUs
	}
	return int(math.Ceil(float64(limits.NanoCPUs)))
}

func memMiBOf(svc types.ServiceConfig) int {
	limits := resourceLimits(svc)
	if limits == nil || limits.MemoryBytes <= 0 {
		return defaultMemMiB
	}
	// Rounded UP, for the reason vcpusOf rounds up: truncating a limit that is
	// not a whole number of MiB hands the guest less than the file asked for,
	// and truncating one under a mebibyte hands it ZERO -- which the machine
	// layer then silently replaces with its own default.
	return int(math.Ceil(float64(limits.MemoryBytes) / mib))
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
