package build

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// What the guest agent needs in order to start the application.
//
// The golden template stops short of starting the application, and the guest
// agent execs it after env delivery (ARCHITECTURE.md). That contract needs
// something to exec, and the tar exporter does not carry one: it emits the
// filesystem and nothing else -- no CMD, no ENTRYPOINT, no WORKDIR, no ENV.
// That is the price of taking the flattened filesystem instead of a layered
// image, and it is the right trade, but it leaves a hole that has to be filled
// somewhere.
//
// So the start spec is read out of the DOCKERFILE and written into the image
// at StartSpecPath. Read honestly: this parses the final stage of the
// Dockerfile in the build context, so it sees what that Dockerfile declares
// and NOTHING the base image declares. `FROM node:24-alpine` with no CMD of
// its own yields an empty spec even though the base image has one.
//
// That limit is recorded in the file rather than left to be discovered:
// FromDockerfileOnly says where these values came from, so a consumer can tell
// "this application declares no start command" from "we could not see one",
// and fall back to whatever the service spec says instead of guessing.

// StartSpecPath is where the spec lands inside the image.
const StartSpecPath = "etc/pilot-agent/start.json"

// StartSpec is the application's declared entry point.
type StartSpec struct {
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	WorkDir    string            `json:"workdir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	// Port is the first EXPOSE, which is a hint and not a promise: a
	// Dockerfile may expose none, or several, or one the process never binds.
	Port int `json:"port,omitempty"`
	// Shell records that the command came from a shell-form CMD or
	// ENTRYPOINT, which Docker runs through `/bin/sh -c`. A consumer that
	// exec's the argv directly would run a program named after the whole
	// command line.
	Shell bool `json:"shell,omitempty"`
	// FromDockerfileOnly says where these values came from. True means they
	// are what the context's Dockerfile declares and nothing else, so an empty
	// Cmd means "not declared in this Dockerfile" rather than "this
	// application has no start command". False means the base image's own
	// config was merged in as well (MergeImageConfig), which is the normal
	// case for a build whose daemon reported one.
	FromDockerfileOnly bool `json:"from_dockerfile_only"`
}

// ImageConfig is the part of an OCI image config that says how to start it.
//
// This is what `FROM postgres:17` carries and a Dockerfile that adds only a
// COPY does not: the tar exporter emits a filesystem, so without this the
// image's own CMD, ENV, WORKDIR and USER are dropped and the machine has
// nothing to run. Recovered from the build's metadata rather than by pulling
// the image again, because the daemon has already resolved it.
type ImageConfig struct {
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	User         string              `json:"User,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
}

// MergeImageConfig fills what the Dockerfile did not declare from the base
// image's config.
//
// Per field, and the Dockerfile always wins the fields it names. That is
// Docker's own rule: an ENTRYPOINT in the Dockerfile replaces the image's, and
// -- the part that is easy to get wrong -- a Dockerfile ENTRYPOINT with no CMD
// of its own DISCARDS the image's CMD, because the image's CMD was written as
// arguments to the image's entrypoint and means nothing to a different one.
//
// Env is the exception that merges key by key rather than wholesale, since a
// Dockerfile's ENV is written as an addition to the base image's environment,
// not as a replacement for it.
func (s StartSpec) MergeImageConfig(cfg *ImageConfig) StartSpec {
	if cfg == nil {
		return s
	}
	used := false

	if len(s.Entrypoint) == 0 && len(s.Cmd) == 0 {
		// Nothing declared here: take both, and the shell form with them. An
		// image config's Cmd and Entrypoint are always exec form by the time
		// they reach a config, so Shell stays false.
		if len(cfg.Entrypoint) > 0 {
			s.Entrypoint, used = append([]string(nil), cfg.Entrypoint...), true
		}
		if len(cfg.Cmd) > 0 {
			s.Cmd, used = append([]string(nil), cfg.Cmd...), true
		}
		if used {
			s.Shell = false
		}
	} else if len(s.Entrypoint) > 0 && len(s.Cmd) == 0 {
		// A new entrypoint and no new arguments. The image's Cmd is NOT
		// inherited: it was arguments to a different program.
	} else if len(s.Entrypoint) == 0 && len(s.Cmd) > 0 && len(cfg.Entrypoint) > 0 {
		// New arguments to the image's own entrypoint, which is what
		// `FROM postgres:17` plus `CMD ["-c", "shared_buffers=256MB"]` means.
		s.Entrypoint, used = append([]string(nil), cfg.Entrypoint...), true
	}

	if s.WorkDir == "" && cfg.WorkingDir != "" {
		s.WorkDir, used = cfg.WorkingDir, true
	}
	if s.User == "" && cfg.User != "" {
		s.User, used = cfg.User, true
	}
	if s.Port == 0 {
		if p := lowestPort(cfg.ExposedPorts); p != 0 {
			s.Port, used = p, true
		}
	}
	if len(cfg.Env) > 0 {
		env := make(map[string]string, len(s.Env)+len(cfg.Env))
		for _, kv := range cfg.Env {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				continue
			}
			env[k], used = v, true
		}
		// The Dockerfile's own ENV lands last, so it wins every key it names.
		for k, v := range s.Env {
			env[k] = v
		}
		s.Env = env
	}

	if used {
		s.FromDockerfileOnly = false
	}
	return s
}

// lowestPort picks one port out of an image's ExposedPorts.
//
// Lowest rather than first: the map has no order, so "first" would differ
// between two runs of the same build and the machine's published port would
// move. An image exposing 5432 and 8080 gets 5432 every time.
func lowestPort(ports map[string]struct{}) int {
	best := 0
	for spec := range ports {
		// "5432/tcp", or bare "5432". UDP is skipped: the router speaks TCP,
		// and a machine whose only exposed port is UDP has no port we can use.
		portStr, proto, hasProto := strings.Cut(spec, "/")
		if hasProto && proto != "tcp" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || n <= 0 || n > 65535 {
			continue
		}
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

// Empty reports a spec that names no way to start anything.
func (s StartSpec) Empty() bool { return len(s.Entrypoint) == 0 && len(s.Cmd) == 0 }

// dockerDefaultPath is the PATH every container runtime gives a process when
// the image config names none. It is in the OCI runtime spec's own defaults
// and in Docker's, which is why a Dockerfile can say `CMD ["node", "x.js"]`
// and mean /usr/local/bin/node.
const dockerDefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// WithRuntimeDefaults fills in what the image config would have carried.
//
// PATH, and only PATH, because it is the one such value whose absence turns a
// correct command into "not found". The guest agent execs the application with
// the environment PID 1 was given, and PID 1 in a microVM is given the
// KERNEL's PATH -- /sbin:/usr/sbin:/bin:/usr/bin, with no /usr/local/bin in
// it. So `node`, `bun`, `python3` and every other interpreter that lives where
// its base image put it is unresolvable, and the machine restart-loops on
// `/bin/sh: exec: line 0: node: not found` while the image, the build and the
// command are all correct.
//
// A Dockerfile's own ENV PATH still wins: this only fills a blank. What it
// cannot see is a PATH the BASE image set and this Dockerfile did not, which
// is the same blind spot FromDockerfileOnly records -- Docker's default is the
// better guess than the kernel's, and it is a guess either way.
func (s StartSpec) WithRuntimeDefaults() StartSpec {
	if _, ok := s.Env["PATH"]; ok {
		return s
	}
	env := make(map[string]string, len(s.Env)+1)
	for k, v := range s.Env {
		env[k] = v
	}
	env["PATH"] = dockerDefaultPath
	s.Env = env
	return s
}

// ParseStartSpec reads the final stage of a Dockerfile.
//
// The FINAL stage, because a multi-stage build's earlier stages describe a
// toolchain that is thrown away -- taking a builder stage's CMD would start
// the compiler instead of the application. Every FROM resets what has been
// collected so far, which is exactly Docker's own rule.
func ParseStartSpec(dockerfile string) StartSpec {
	spec := StartSpec{FromDockerfileOnly: true}

	for _, line := range joinContinuations(dockerfile) {
		instr, rest, ok := splitInstruction(line)
		if !ok {
			continue
		}
		switch instr {
		case "FROM":
			// A new stage. Everything collected belonged to the previous one.
			spec = StartSpec{FromDockerfileOnly: true}
		case "ENTRYPOINT":
			args, shell := parseArgv(rest)
			spec.Entrypoint = args
			spec.Shell = spec.Shell || shell
		case "CMD":
			args, shell := parseArgv(rest)
			spec.Cmd = args
			spec.Shell = spec.Shell || shell
		case "WORKDIR":
			spec.WorkDir = strings.Trim(strings.TrimSpace(rest), `"'`)
		case "USER":
			spec.User = strings.TrimSpace(rest)
		case "ENV":
			for k, v := range parseEnv(rest) {
				if spec.Env == nil {
					spec.Env = map[string]string{}
				}
				spec.Env[k] = v
			}
		case "EXPOSE":
			if spec.Port == 0 {
				spec.Port = firstPort(rest)
			}
		}
	}
	return spec
}

// joinContinuations folds backslash-continued lines and drops comments.
func joinContinuations(dockerfile string) []string {
	var out []string
	var current strings.Builder

	for _, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// A comment inside a continuation is dropped by Docker too, so it must
		// not terminate the instruction being assembled.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			current.WriteString(strings.TrimSuffix(trimmed, `\`))
			current.WriteString(" ")
			continue
		}
		current.WriteString(trimmed)
		if s := strings.TrimSpace(current.String()); s != "" {
			out = append(out, s)
		}
		current.Reset()
	}
	if s := strings.TrimSpace(current.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// splitInstruction separates the keyword from its arguments. Instructions are
// case-insensitive in a Dockerfile and are conventionally upper case; both are
// accepted because both are legal.
func splitInstruction(line string) (instr, rest string, ok bool) {
	fields := strings.SplitN(line, " ", 2)
	if len(fields) == 0 || fields[0] == "" {
		return "", "", false
	}
	instr = strings.ToUpper(fields[0])
	if len(fields) == 2 {
		rest = fields[1]
	}
	// Flags like `--platform=` on FROM, or `--chown=` on COPY, are not part of
	// anything read here.
	return instr, rest, true
}

// parseArgv reads a CMD or ENTRYPOINT in either form.
//
// Exec form is a JSON array and is exec'd directly. Shell form is a bare
// command line that Docker runs through `/bin/sh -c`, and the difference
// matters at the moment of exec: running a shell-form command as argv would
// try to exec a program whose name is the entire command line, signals would
// not reach the real process, and no redirection or variable would expand.
func parseArgv(rest string) (args []string, shell bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil, false
	}
	if strings.HasPrefix(rest, "[") {
		var parsed []string
		if err := json.Unmarshal([]byte(rest), &parsed); err == nil {
			return parsed, false
		}
		// A malformed array is not silently reinterpreted as shell form: that
		// would run something subtly different from what was written.
		return nil, false
	}
	return []string{rest}, true
}

// parseEnv handles both `ENV k=v k2=v2` and the legacy `ENV k v`.
func parseEnv(rest string) map[string]string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return nil
	}
	out := map[string]string{}

	if !strings.Contains(rest, "=") {
		// Legacy form: everything after the first space is one value.
		parts := strings.SplitN(rest, " ", 2)
		if len(parts) == 2 {
			out[parts[0]] = strings.TrimSpace(parts[1])
		}
		return out
	}
	for _, pair := range splitRespectingQuotes(rest) {
		k, v, found := strings.Cut(pair, "=")
		if !found || k == "" {
			continue
		}
		out[k] = strings.Trim(v, `"'`)
	}
	return out
}

// splitRespectingQuotes splits on spaces that are not inside quotes, so
// `ENV GREETING="hello world" TZ=UTC` stays two entries.
func splitRespectingQuotes(s string) []string {
	var out []string
	var current strings.Builder
	var quote rune

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			current.WriteRune(r)
		case r == ' ':
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

// firstPort reads the first EXPOSE, ignoring any /tcp or /udp suffix.
func firstPort(rest string) int {
	for _, field := range strings.Fields(rest) {
		port, _, _ := strings.Cut(field, "/")
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return 0
}

// Marshal renders the spec for the image.
func (s StartSpec) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("build: encode the start spec: %w", err)
	}
	return append(raw, '\n'), nil
}
