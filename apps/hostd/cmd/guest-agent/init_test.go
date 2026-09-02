package main

import (
	"errors"
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
		"MULTLINE=\"one\ntwo\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env file does not contain %s\ngot:\n%s", want, got)
		}
	}

	// A newline inside a quoted value is written as ITSELF, and this test
	// used to assert the opposite -- one line per variable, with newlines
	// escaped as \n. That is what a shell would read, and systemd is not a
	// shell: its EnvironmentFile parser takes the byte after a backslash
	// literally and has no \n, so the escaped form reached the application
	// as a literal backslash and n. Verified against real systemd:
	//
	//	A="one\ntwo"     -> A=one\ntwo      (wrong: 8 characters)
	//	B="real<LF>nl"   -> B=real<LF>nl    (right)
	//
	// So the multi-line value is expected to span lines, and the file has
	// more lines than it has variables.
	if !strings.Contains(got, "MULTLINE=\"one\ntwo\"") {
		t.Errorf("the newline was not written literally:\n%s", got)
	}
	if strings.Contains(got, `\n`) {
		t.Errorf("a value was escaped as \\n, which systemd does not decode:\n%s", got)
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

// stubStart replaces the real start with a recorder.
func stubStart(t *testing.T, started bool, reason string) *int {
	t.Helper()
	calls := 0
	old := startApp
	startApp = func() (bool, string) { calls++; return started, reason }
	t.Cleanup(func() { startApp = old })
	return &calls
}

// The asymmetry, from the guest's end.
//
// hostd pokes /init after EVERY resume to unfreeze the wall clock, and that
// poke carries a timestamp and nothing else. So a wake has to arrive here and
// change nothing: not the environment file, not the command, and above all not
// the running process. Re-execing there restarts the very process the guest
// just spent its restore bringing back, and the machine looks perfectly
// healthy afterwards.
func TestAWakeShapedInitChangesNothing(t *testing.T) {
	redirect(t)
	calls := stubStart(t, true, "")

	// A create first: environment delivered, command recorded, application
	// started.
	resp, err := applyInit(initRequest{
		TimestampNanos: 1,
		Env:            map[string]string{"GREETING": "hello"},
		AppCmd:         "/usr/bin/server",
		StartApp:       true,
	})
	if err != nil {
		t.Fatalf("create-shaped init: %v", err)
	}
	if !resp.AppStarted || *calls != 1 {
		t.Fatalf("a create did not start the application (started=%v calls=%d)",
			resp.AppStarted, *calls)
	}
	envBefore, _ := os.ReadFile(envPath)
	appBefore, _ := os.ReadFile(appPath)

	// Now the wake: a timestamp and nothing else, which is exactly what
	// pokeGuestClock sends.
	resp, err = applyInit(initRequest{TimestampNanos: 2})
	if err != nil {
		t.Fatalf("wake-shaped init: %v", err)
	}

	if *calls != 1 {
		t.Errorf("a wake tried to start the application; env delivery belongs " +
			"on create and on nothing else")
	}
	if resp.AppStarted {
		t.Error("a wake reported starting the application")
	}
	envAfter, _ := os.ReadFile(envPath)
	if string(envAfter) != string(envBefore) {
		t.Errorf("a wake rewrote the environment:\nbefore %q\nafter  %q", envBefore, envAfter)
	}
	appAfter, _ := os.ReadFile(appPath)
	if string(appAfter) != string(appBefore) {
		t.Errorf("a wake rewrote the application command:\nbefore %q\nafter %q",
			appBefore, appAfter)
	}
}

// The guard that makes the mistake visible rather than silent. If a start is
// ever asked for while the application is running, the host is told so and
// logs it -- on a create that can only mean the golden template stopped
// stopping short of starting applications.
func TestStartingAnAlreadyRunningApplicationIsRefusedAndReported(t *testing.T) {
	redirect(t)
	if err := writeAppCmd("/usr/bin/server"); err != nil {
		t.Fatal(err)
	}

	var asked []string
	old := run
	run = func(name string, args ...string) error {
		asked = append(asked, name+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] == "is-active" {
			return nil // already running
		}
		return nil
	}
	t.Cleanup(func() { run = old })

	started, reason := startApp()
	if started {
		t.Error("a running application was restarted")
	}
	if !strings.Contains(reason, "already running") {
		t.Errorf("reason is %q, which does not say what happened", reason)
	}
	for _, cmd := range asked {
		if strings.Contains(cmd, "start ") {
			t.Errorf("systemctl start was issued anyway: %q", cmd)
		}
	}
}

// A start that genuinely fails must say so rather than reporting success: the
// machine is up and serving either way, so nothing else would notice.
func TestAFailedStartIsReported(t *testing.T) {
	redirect(t)
	if err := writeAppCmd("/usr/bin/server"); err != nil {
		t.Fatal(err)
	}

	old := run
	run = func(name string, args ...string) error {
		if len(args) > 0 && args[0] == "is-active" {
			return errors.New("inactive")
		}
		return errors.New("Unit pilot-app.service not found")
	}
	t.Cleanup(func() { run = old })

	started, reason := startApp()
	if started {
		t.Error("a failed start reported success")
	}
	if !strings.Contains(reason, "not found") {
		t.Errorf("reason is %q and does not carry systemd's own words", reason)
	}
}
