package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The application a built image runs, and how the agent learns it.
//
// Two kinds of image reach this code and they carry the start command in
// different places, which is the seam this file exists to close.
//
// The golden template is built by us. A create passes the command explicitly
// and the agent writes it to /etc/pilot/app, where pilot-app.service reads it.
// That path needs nothing from here.
//
// An image built from a user's Dockerfile is not ours, and BuildKit's tar
// exporter emits the flattened filesystem and NOTHING else -- no CMD, no
// ENTRYPOINT, no WORKDIR, no ENV. That is the price of not taking the layered
// oci export, and it is the right price, but it means the start command has to
// be carried separately: the build path parses the Dockerfile's final stage
// and writes it to /etc/pilot-agent/start.json. Nothing read that file until
// now. A machine created from a user build came up, answered health checks,
// reported app_started, and ran no application at all -- the failure looked
// exactly like success.
//
// So: an explicit command wins, and the build's declared spec is the fallback.

// startSpecPath is where the build path leaves what it parsed. It must match
// build.StartSpecPath; the two are deliberately not a shared constant, because
// this binary is copied INTO an image and cannot import hostd's packages.
var startSpecPath = "/etc/pilot-agent/start.json"

// startSpec is the subset of the build's spec the agent acts on. The build
// writes more fields than this (a port hint, a provenance flag); reading only
// what is used keeps an unknown future field from breaking the decode.
type startSpec struct {
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`
	WorkDir    string            `json:"workdir"`
	Env        map[string]string `json:"env"`
	User       string            `json:"user"`
	// Shell records that the command came from a shell-form CMD or ENTRYPOINT,
	// which Docker runs through /bin/sh -c. Running such a command as argv
	// execs a program named after the whole command line, and no redirection
	// or variable expands.
	Shell bool `json:"shell"`
}

// readStartSpec loads the build's start spec, if the image carries one.
//
// A missing file is not an error: the golden template has no build behind it,
// and an image whose Dockerfile declared no CMD legitimately has nothing here.
// Both cases mean "no command from this source", which is a different answer
// from "this image is broken".
func readStartSpec() (*startSpec, bool) {
	raw, err := os.ReadFile(startSpecPath)
	if err != nil {
		return nil, false
	}
	var s startSpec
	if err := json.Unmarshal(raw, &s); err != nil {
		// A corrupt spec is worth saying out loud. It means the build wrote
		// something this agent cannot read, and the machine is about to come
		// up running nothing -- which is the exact silent failure above.
		fmt.Fprintf(os.Stderr, "guest-agent: %s is unreadable: %v\n", startSpecPath, err)
		return nil, false
	}
	if len(s.Entrypoint) == 0 && len(s.Cmd) == 0 {
		return nil, false
	}
	return &s, true
}

// Command renders the spec as a single shell command line.
//
// One string rather than an argv, because that is what pilot-app.service and
// the supervisor both consume, and because Docker's own composition rules --
// ENTRYPOINT is the program and CMD is its default arguments, with either
// permitted in shell form -- collapse cleanly into one line.
//
// Shell form is passed through verbatim: it was written to be interpreted, and
// quoting it would defeat the redirection and expansion it exists for. Exec
// form is quoted argument by argument, so a path with a space stays one
// argument.
func (s *startSpec) Command() string {
	parts := append(append([]string{}, s.Entrypoint...), s.Cmd...)
	if len(parts) == 0 {
		return ""
	}
	if s.Shell {
		return strings.Join(parts, " ")
	}
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		quoted = append(quoted, shellQuote(p))
	}
	return strings.Join(quoted, " ")
}

// shellQuote makes one argument survive /bin/sh -c intact.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// Anything outside this set is safe to leave bare, and leaving the common
	// case bare keeps the rendered command readable in logs and in ps.
	if !strings.ContainsAny(s, " \t\n\r\"'\\$`&|;<>()*?[]{}~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readAppCmd reads back what applyInit wrote, so the two start mechanisms
// consume one source of truth rather than each re-deriving the command.
//
// The files are the unit's EnvironmentFile format -- KEY=value, one per line,
// values single-quoted when they need it -- because pilot-app.service reads
// them directly and the supervisor must not disagree with it about what the
// application runs.
func readAppCmd() (string, map[string]string, error) {
	appRaw, err := os.ReadFile(appPath)
	if err != nil {
		return "", nil, err
	}
	cmd := unquoteEnvValue(strings.TrimPrefix(strings.TrimSpace(string(appRaw)), "PILOT_APP_CMD="))

	env := map[string]string{}
	// A machine can have a command and no environment; the reverse is nothing
	// to start. So a missing env file is not an error here.
	if envRaw, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(envRaw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			env[k] = unquoteEnvValue(v)
		}
	}
	return cmd, env, nil
}

// unquoteEnvValue reverses quoteEnvValue, whose escaping is systemd's
// EnvironmentFile format: the value is wrapped in double quotes with
// backslash, quote, newline and carriage return escaped inside it.
//
// It must stay the exact inverse. The supervisor and pilot-app.service read
// the same two files, so a disagreement here means one mechanism runs a
// different command from the other -- on the same machine, decided by whether
// the image happened to carry systemd.
func unquoteEnvValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	inner := v[1 : len(v)-1]

	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\\' || i+1 >= len(inner) {
			b.WriteByte(inner[i])
			continue
		}
		i++
		switch inner[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		default:
			// Covers the backslash and double-quote cases, and leaves any
			// other escaped byte as itself rather than inventing a meaning.
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}
