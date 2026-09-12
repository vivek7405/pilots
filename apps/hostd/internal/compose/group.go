package compose

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
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
		if step.Build == nil {
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
	merged.Ports, merged.Health = ports, health
	merged.Replicas, merged.VCPUs, merged.MemMiB = replicas, vcpus, mem
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
	for _, line := range strings.Split(s.DockerfileAppend, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "CMD ")
		if !ok {
			rest, ok = strings.CutPrefix(line, "ENTRYPOINT ")
		}
		if !ok {
			continue
		}
		return shellFromJSONArray(strings.TrimSpace(rest))
	}
	return ""
}

// shellFromJSONArray turns `["sh","-c","x"]` into a shell string. A value that
// is not a JSON array is already one.
func shellFromJSONArray(v string) string {
	if !strings.HasPrefix(v, "[") {
		return v
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, `"`)
		p = strings.TrimSuffix(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
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
