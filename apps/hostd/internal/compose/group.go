package compose

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/pilotsrun/pilots/hostd/internal/api"
)

// ProcessesEnv is the environment variable a machine's process set travels in.
//
// Read by the guest agent at init (cmd/guest-agent). The name is deliberately
// in the PILOT_ namespace the agent already strips from what the application
// sees, so an app cannot read it by accident and cannot set it on purpose.
const ProcessesEnv = "PILOT_PROCESSES"

// Several compose services on one build context become one machine.
//
// # Why this reading and not fly's
//
// A compose file that builds a web server and a worker from the same directory
// is describing two commands over one filesystem. Run as two machines, that
// filesystem is built twice, shipped twice, cached twice and paid for twice,
// and the two halves can no longer see each other's local sockets or files.
// Run as one machine with two processes, it is what the author wrote.
//
// Services with DIFFERENT images stay separate machines, and that is the
// deliberate divergence from fly's compose reading, which puts several
// containers in one Machine. Two images in one machine needs a container
// runtime inside the guest, which this project does not have and is not adding
// (docs/prior-art/fly-io.md, and ARCHITECTURE.md's reading of it). Keeping them
// separate also preserves what compose authors already rely on: service-name
// networking over .internal, a volume per service, and suspend per service.
//
// So the rule is mechanical: same build context and same Dockerfile means one
// machine; anything else means one machine each. Nothing is guessed.

// groupByContext folds services that share a build context into one step each.
//
// The returned map is keyed by the surviving step's name, which is the
// alphabetically first member. Deterministic on purpose: a plan that named the
// machine after whichever service the map happened to yield first would give
// two runs of one file two different machine names, and a machine's name is
// its URL.
func groupByContext(steps map[string]Step) (map[string]Step, error) {
	// Bucket by context, in name order so the survivor is stable.
	names := make([]string, 0, len(steps))
	for name := range steps {
		names = append(names, name)
	}
	sort.Strings(names)

	buckets := map[string][]string{}
	var order []string
	for _, name := range names {
		step := steps[name]
		if step.Build == nil || step.SeparateMachine {
			continue
		}
		key := step.Build.Context + "\x00" + step.Build.Dockerfile
		if _, seen := buckets[key]; !seen {
			order = append(order, key)
		}
		buckets[key] = append(buckets[key], name)
	}

	out := make(map[string]Step, len(steps))
	grouped := map[string]bool{}
	for _, key := range order {
		members := buckets[key]
		if len(members) < 2 {
			continue
		}
		// Two members that both PUBLISH are two machines, not one refusal.
		//
		// Grouping is an optimisation over what compose already means: each
		// service is its own instance, and folding several onto one machine is
		// worth doing when they are several commands over one filesystem. Two
		// servers are not that. One machine has one address, so a machine
		// cannot hold both -- and the honest answer to "which of these owns the
		// port" is that neither does, because they were never one machine.
		//
		// This used to refuse the whole plan, and the file it refuses is the
		// one that matters most: webjs's own compose.pilots.yaml deploys
		// `website` and `gallery` from one build context, both on 8080, and
		// `pilot deploy` answered "Only one process can own the machine's port"
		// and stopped. The project's own dogfood file could not be deployed by
		// the project. Declining to group costs the second machine's rootfs --
		// which the build cache already shares -- and buys a deploy that works.
		if publishers(steps, members) > 1 {
			continue
		}
		merged, err := mergeMembers(steps, members)
		if err != nil {
			return nil, err
		}
		out[merged.Name] = merged
		for _, name := range members {
			grouped[name] = true
		}
	}
	for _, name := range names {
		if !grouped[name] {
			out[name] = steps[name]
		}
	}
	return out, nil
}

// publishers counts how many of these services declare a port.
//
// The count, not a boolean, because one publisher is the ordinary grouped case
// -- a web server and its worker -- and it is only the SECOND that makes a
// machine impossible.
func publishers(steps map[string]Step, members []string) int {
	n := 0
	for _, name := range members {
		if len(steps[name].Ports) > 0 {
			n++
		}
	}
	return n
}

// mergeMembers folds several services into one step with several processes.
func mergeMembers(steps map[string]Step, members []string) (Step, error) {
	merged := steps[members[0]]
	merged.Name = members[0]
	inGroup := map[string]bool{}
	for _, name := range members {
		inGroup[name] = true
	}

	// Env is the UNION, and a key two members disagree about is refused rather
	// than resolved. One machine has one environment: silently taking one
	// service's DATABASE_URL over another's would be a data-loss bug that
	// looks like a deploy that worked.
	env := map[string]string{}
	secrets := map[string]string{}
	var ports []int
	var health = merged.Health
	var dependsOn []string
	var processes []Process
	// Fields that belong to the MACHINE rather than to a process, collected
	// across every member.
	//
	// merged starts as a copy of members[0], so anything not gathered here is
	// silently the alphabetically-first member's answer and every other
	// member's is discarded. Volumes is the one that costs data: a compose
	// file with `web` and `worker` on one build context, where worker declares
	// a volume, deployed one machine with web's volumes -- none -- and
	// everything the worker wrote went to the copy-on-write rootfs and died
	// with the next redeploy. Nothing said a volume had been dropped.
	volumes := map[string]Volume{}
	var knobs *api.Knobs
	var snapshotPolicy *api.VolumePolicy
	private, customDomain, preDeploy := merged.Private, merged.CustomDomain, merged.PreDeploy
	replicas, vcpus, mem := 0, 0, 0
	portOwner := ""

	for _, name := range members {
		s := steps[name]
		for k, v := range s.Env {
			if old, dup := env[k]; dup && old != v {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context both set %s, to different values; they run as one "+
					"machine and cannot have two environments. Give them different "+
					"build contexts, or set %s once", name, k, k)
			}
			env[k] = v
		}
		for k, v := range s.SecretRefs {
			if old, dup := secrets[k]; dup && old != v {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context both set %s from different secrets", name, k)
			}
			secrets[k] = v
		}
		if len(s.Ports) > 0 {
			if portOwner != "" && portOwner != name {
				return Step{}, fmt.Errorf("compose: %s and %s share a build context, so "+
					"they run as one machine, and both publish ports. Only one process "+
					"can own the machine's port: publish from one of them, or give them "+
					"different build contexts", portOwner, name)
			}
			portOwner = name
			ports = s.Ports
			if s.Health != nil {
				health = s.Health
			}
		}
		// A dependency on a service OUTSIDE the group stays on the step, where
		// the rollout orders whole machines. One inside becomes a process
		// ordering, which the guest agent applies at start.
		var needs []string
		for _, dep := range s.DependsOn {
			if inGroup[dep] {
				needs = append(needs, dep)
				continue
			}
			dependsOn = append(dependsOn, dep)
		}
		processes = append(processes, Process{
			Name: name, Cmd: commandOf(s), Needs: needs, Port: len(s.Ports) > 0,
		})
		// A volume is the machine's, so every member's travels. Two members
		// naming one mount path with different volumes is refused for the
		// reason two environments are: one machine has one filesystem, and
		// silently taking one is a data-loss bug wearing a successful deploy.
		for _, v := range s.Volumes {
			if old, dup := volumes[v.MountPath]; dup && old != v {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context both mount %s, from different volumes; they run as "+
					"one machine and cannot have two. Mount it once, or give them "+
					"different build contexts", name, v.MountPath)
			}
			volumes[v.MountPath] = v
		}
		// The rest of the machine's own policy, first writer wins and a
		// disagreement is refused. These were dropped outright before.
		if s.Knobs != nil {
			if knobs != nil && !reflect.DeepEqual(*knobs, *s.Knobs) {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context set different lifecycle knobs; they run as one "+
					"machine and have one policy", name)
			}
			knobs = s.Knobs
		}
		if s.SnapshotPolicy != nil {
			if snapshotPolicy != nil && !reflect.DeepEqual(*snapshotPolicy, *s.SnapshotPolicy) {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context set different snapshot policies", name)
			}
			snapshotPolicy = s.SnapshotPolicy
		}
		if s.CustomDomain != "" {
			if customDomain != "" && customDomain != s.CustomDomain {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context set different custom domains; one machine has one "+
					"address", name)
			}
			customDomain = s.CustomDomain
		}
		if s.PreDeploy != "" {
			if preDeploy != "" && preDeploy != s.PreDeploy {
				return Step{}, fmt.Errorf("compose: %s and another service on the same "+
					"build context set different pre-deploy commands", name)
			}
			preDeploy = s.PreDeploy
		}
		// Private is the conservative direction: a group is private only when
		// every member is, because one member that serves needs an address.
		private = private && s.Private

		// The machine is sized to hold every process, so the largest wins
		// rather than the first.
		if s.Replicas > replicas {
			replicas = s.Replicas
		}
		if s.VCPUs > vcpus {
			vcpus = s.VCPUs
		}
		if s.MemMiB > mem {
			mem = s.MemMiB
		}
	}

	// The processes reach the guest through the environment it is already
	// handed at init, as one JSON value, rather than through a new field on
	// the service row and a new delivery path beside it.
	//
	// That is not a shortcut. Env is re-delivered on every create, wake and
	// restore, which is exactly when a machine has to know what to run, and it
	// is already the mechanism that survives a snapshot. A column on the
	// service row would be a second contract to keep correct and a Corrosion
	// shape change (rule 6) for a value the guest reads once at start.
	if len(processes) > 0 {
		raw, err := json.Marshal(processes)
		if err != nil {
			return Step{}, fmt.Errorf("compose: encoding the processes of %s: %w", merged.Name, err)
		}
		env[ProcessesEnv] = string(raw)
	}
	if len(env) > 0 {
		merged.Env = env
	}
	if len(secrets) > 0 {
		merged.SecretRefs = secrets
	}
	// Every member needs a command of its own, and this is checked LAST so a
	// file with a real conflict in it reports the conflict rather than this.
	//
	// A process entry with an empty command is one the guest agent rejects on
	// sight, and it rejects the whole list: the machine starts NOTHING, not
	// this member and not the others, and the message names no service and
	// says nothing about compose. This one names the service and the fix.
	//
	// There is no honest fallback. Every member of a group shares one image,
	// so "inherit the image's own CMD" can be right for at most one of them
	// and nothing says which; a member that runs the image default has to
	// spell that default out.
	for _, proc := range processes {
		if proc.Cmd == "" {
			return Step{}, fmt.Errorf("compose: %s shares a build context with another "+
				"service, so they run as one machine with one process each, and it "+
				"declares no command. Give it a `command:` (or an `entrypoint:`): a "+
				"group cannot fall back to the image's own, because every member "+
				"shares one image and only one of them could have it", proc.Name)
		}
	}

	merged.Ports, merged.Health = ports, health
	merged.Replicas, merged.VCPUs, merged.MemMiB = replicas, vcpus, mem
	merged.Volumes = sortedVolumes(volumes)
	merged.Knobs, merged.SnapshotPolicy = knobs, snapshotPolicy
	merged.Private, merged.CustomDomain, merged.PreDeploy = private, customDomain, preDeploy
	merged.DependsOn = dedupeSorted(dependsOn)
	merged.Processes = processes
	// The grouped step's own command is not one of the processes: each process
	// carries its own. Clearing it keeps the image's start spec from starting
	// one of them twice.
	merged.DockerfileAppend = overridesWithoutCommand(merged.DockerfileAppend)
	return merged, nil
}

// commandOf recovers a step's command from the Dockerfile instructions the
// planner rendered for it.
//
// Read back out of that text rather than kept beside it, because the text is
// already the one place a service's command lives: it is what the build parses
// and what the guest execs, and a second copy would be a second thing to keep
// correct. An empty answer means the service declared no command, so the
// process runs whatever the image's own CMD is.
func commandOf(s Step) string {
	// The append FIRST, because it is what the compose file overrode and an
	// override beats what the image declared. Then the generated Dockerfile,
	// which is where an `image:` step's own CMD lives -- omitting it meant a
	// member that named an image and no command came out with an empty
	// command, which is the case that starts nothing.
	if cmd := lastCommandIn(s.DockerfileAppend); cmd != "" {
		return cmd
	}
	return lastCommandIn(s.Dockerfile)
}

// lastCommandIn is the last CMD or ENTRYPOINT in a Dockerfile fragment,
// rendered as a shell string.
//
// LAST, not first, because that is Docker's own rule: a later CMD replaces an
// earlier one. Reading the first meant a fragment that overrode its own
// command handed back the line the override was replacing.
func lastCommandIn(text string) string {
	out := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "CMD ")
		if !ok {
			rest, ok = strings.CutPrefix(line, "ENTRYPOINT ")
		}
		if !ok {
			continue
		}
		out = shellFromJSONArray(strings.TrimSpace(rest))
	}
	return out
}

// sortedVolumes is the gathered volumes in mount-path order, so a plan is the
// same on every host that renders it.
func sortedVolumes(byPath map[string]Volume) []Volume {
	if len(byPath) == 0 {
		return nil
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]Volume, 0, len(paths))
	for _, path := range paths {
		out = append(out, byPath[path])
	}
	return out
}

// shellFromJSONArray turns `["sh","-c","x"]` into a shell string. A value that
// is not a JSON array is already one.
//
// PARSED as JSON and re-quoted, not split on commas. Splitting destroyed any
// argument containing a space or a comma, and the postgres recipe has one:
// archive_command=test ! -f /archive/wal/%f && cp %p /archive/wal/%f. Flattened
// naively that becomes a string the guest's `sh -c` then splits on &&, so
// postgres was exec'd with three stray argv entries and died with "too many
// command-line arguments". recipes.go says a LIST was chosen precisely because
// "a string would be re-split by whatever runs it"; this is the code that was
// re-splitting it.
func shellFromJSONArray(v string) string {
	if !strings.HasPrefix(v, "[") {
		return v
	}
	var argv []string
	if err := json.Unmarshal([]byte(v), &argv); err != nil || len(argv) == 0 {
		// Not a JSON array after all. Handed back as written, which is what a
		// shell-form CMD already is.
		return v
	}
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shellQuote(a))
	}
	return strings.Join(out, " ")
}

// shellQuote makes one argv element survive `sh -c`.
//
// Single quotes, because inside them a shell expands nothing at all -- no $,
// no backtick, no backslash. An embedded single quote is closed, escaped and
// reopened, which is the standard form and the only case that needs thought.
// A word of plain characters is left bare so an ordinary command still reads
// like one in a log.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// overridesWithoutCommand drops CMD and ENTRYPOINT from rendered overrides,
// keeping WORKDIR, USER and ENV, which apply to every process in the machine.
func overridesWithoutCommand(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CMD ") || strings.HasPrefix(trimmed, "ENTRYPOINT ") {
			continue
		}
		if trimmed == "" {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
