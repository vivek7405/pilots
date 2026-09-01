package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redirect points the env and app paths at a temporary directory.
func redirect(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	oldEnv, oldApp := envPath, appPath
	envPath = filepath.Join(dir, "env")
	appPath = filepath.Join(dir, "app")
	t.Cleanup(func() { envPath, appPath = oldEnv, oldApp })
	return dir
}

// A value with a space, a '#' or a quote in it is ordinary in a connection
// string. Written unquoted, systemd reads back a truncated secret and the
// application fails somewhere entirely unrelated.
func TestEnvValuesAreQuotedForSystemd(t *testing.T) {
	redirect(t)

	if err := writeEnv(map[string]string{
		"PLAIN":    "value",
		"SPACED":   "two words",
		"HASHED":   "pass#word",
		"QUOTED":   `say "hi"`,
		"SLASHED":  `a\b`,
		"MULTLINE": "one\ntwo",
	}); err != nil {
		t.Fatalf("writeEnv: %v", err)
	}

	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	for _, want := range []string{
		`PLAIN="value"`,
		`SPACED="two words"`,
		`HASHED="pass#word"`,
		`QUOTED="say \"hi\""`,
		`SLASHED="a\\b"`,
		`MULTLINE="one\ntwo"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file does not contain %s\ngot:\n%s", want, got)
		}
	}
	// One line per variable: a literal newline inside a value would make
	// systemd read the rest of the secret as another variable.
	if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 6 {
		t.Errorf("env file has %d lines for 6 variables:\n%s", lines, got)
	}
}

// The file holds every secret the machine was deployed with, and the workload
// runs as a real user with a shell.
func TestTheEnvFileIsNotReadableByTheWorkload(t *testing.T) {
	redirect(t)
	if err := writeEnv(map[string]string{"SECRET": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file is mode %o, want 600", perm)
	}
}

// A wake sends no env at all. Rewriting the file with nothing in it would
// strip a running application of the environment it started with, which is
// the failure this whole asymmetry exists to prevent -- arriving from the
// other direction.
func TestNoEnvLeavesTheExistingFileAlone(t *testing.T) {
	redirect(t)
	if err := writeEnv(map[string]string{"KEEP": "me"}); err != nil {
		t.Fatal(err)
	}
	if err := writeEnv(nil); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(envPath)
	if !strings.Contains(string(raw), `KEEP="me"`) {
		t.Errorf("an init with no env erased the existing one: %q", raw)
	}
}

// A name that is not a shell variable name would make systemd read the file as
// something other than what was meant. Refused rather than written.
func TestInvalidEnvNamesAreRefused(t *testing.T) {
	redirect(t)
	for _, name := range []string{"", "1LEADING", "has space", "has=equals", "has\nnewline"} {
		if err := writeEnv(map[string]string{name: "x"}); err == nil {
			t.Errorf("the name %q was accepted", name)
		}
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Error("a refused environment still wrote a file")
	}
}

func TestAppCommandMayNotSpanLines(t *testing.T) {
	redirect(t)
	if err := writeAppCmd("start\nrm -rf /"); err == nil {
		t.Error("a multi-line application command was accepted")
	}
	if err := writeAppCmd("/usr/bin/node server.js"); err != nil {
		t.Fatalf("writeAppCmd: %v", err)
	}
	raw, _ := os.ReadFile(appPath)
	if got, want := strings.TrimSpace(string(raw)), `PILOT_APP_CMD="/usr/bin/node server.js"`; got != want {
		t.Errorf("app file is %q, want %q", got, want)
	}
}

// A machine that carries no application command is a bare sandbox. It must
// report that rather than look like a failed start.
func TestStartingWithNoApplicationCommandSaysSo(t *testing.T) {
	redirect(t)
	started, reason := startApp()
	if started {
		t.Error("an application was started with no command to start")
	}
	if !strings.Contains(reason, "no application command") {
		t.Errorf("reason is %q", reason)
	}
}
