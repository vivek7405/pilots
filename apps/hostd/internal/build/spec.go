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
	// FromDockerfileOnly is always true and is there to be read. These values
	// are what the context's Dockerfile declares; anything inherited from the
	// base image is invisible here, so an empty Cmd means "not declared in
	// this Dockerfile", never "this application has no start command".
	FromDockerfileOnly bool `json:"from_dockerfile_only"`
}

// Empty reports a spec that names no way to start anything.
func (s StartSpec) Empty() bool { return len(s.Entrypoint) == 0 && len(s.Cmd) == 0 }

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
