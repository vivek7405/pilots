package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withSpec points the agent at a start spec written to a temp dir, and at
// temp app/env files, so a test can drive applyInit without touching /etc.
func withSpec(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()

	specPath := filepath.Join(dir, "start.json")
	if body != "" {
		if err := os.WriteFile(specPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldSpec, oldApp, oldEnv := startSpecPath, appPath, envPath
	oldWorkDir, oldUser := appWorkDir, appUser
	startSpecPath = specPath
	appPath = filepath.Join(dir, "app")
	envPath = filepath.Join(dir, "env")
	appWorkDir, appUser = "", ""
	t.Cleanup(func() {
		startSpecPath, appPath, envPath = oldSpec, oldApp, oldEnv
		appWorkDir, appUser = oldWorkDir, oldUser
	})
	return dir
}

// The seam this file exists to close.
//
// The build path writes what a Dockerfile declared to /etc/pilot-agent/start.json
// and nothing read it. A machine created from a user's build came up, answered
// health checks, reported app_started, and ran no application -- the failure was
// indistinguishable from success from outside the guest.
func TestACreateWithNoCommandFallsBackToTheBuildSpec(t *testing.T) {
	withSpec(t, `{
	  "entrypoint": ["node"],
	  "cmd": ["server.js"],
	  "workdir": "/app",
	  "env": {"PORT": "8080"},
	  "from_dockerfile_only": true
	}`)

	// A create that carries no command of its own, which is every create from
	// a built image: hostd has nothing to pass, because the command lives in
	// the image rather than in the machine row.
	if _, err := applyInit(initRequest{}); err != nil {
		t.Fatal(err)
	}

	cmd, env, err := readAppCmd()
	if err != nil {
		t.Fatalf("nothing was written for the app to start: %v", err)
	}
	if cmd != "node server.js" {
		t.Errorf("command = %q, want %q", cmd, "node server.js")
	}
	if env["PORT"] != "8080" {
		t.Errorf("the Dockerfile's ENV did not reach the application: %v", env)
	}
	if appWorkDir != "/app" {
		t.Errorf("workdir = %q, want /app", appWorkDir)
	}
}

// An explicit command is the machine's own and must win: a deploy that sets a
// command means it, and silently running the image's instead would be a
// machine doing something nobody asked for.
func TestAnExplicitCommandBeatsTheBuildSpec(t *testing.T) {
	withSpec(t, `{"cmd": ["from-the-image"], "from_dockerfile_only": true}`)

	if _, err := applyInit(initRequest{AppCmd: "from-the-create"}); err != nil {
		t.Fatal(err)
	}
	cmd, _, err := readAppCmd()
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "from-the-create" {
		t.Errorf("command = %q; the explicit command must win", cmd)
	}
}

// The create's environment overrides the image's on a collision, for the same
// reason: it is the later and more specific statement of intent.
func TestTheCreateEnvironmentOverridesTheImages(t *testing.T) {
	withSpec(t, `{"cmd": ["x"], "env": {"PORT": "8080", "MODE": "image"}}`)

	if _, err := applyInit(initRequest{Env: map[string]string{"MODE": "deploy"}}); err != nil {
		t.Fatal(err)
	}
	_, env, err := readAppCmd()
	if err != nil {
		t.Fatal(err)
	}
	if env["MODE"] != "deploy" {
		t.Errorf("MODE = %q, want the create's value", env["MODE"])
	}
	if env["PORT"] != "8080" {
		t.Errorf("PORT = %q; a non-colliding image value must survive", env["PORT"])
	}
}

// Shell form is what Docker runs through /bin/sh -c. Quoting it would defeat
// the redirection and expansion it was written for; running exec form
// unquoted would let a path with a space become two arguments.
func TestShellAndExecFormRenderDifferently(t *testing.T) {
	shell := (&startSpec{Cmd: []string{"node server.js > /var/log/app.log"}, Shell: true}).Command()
	if shell != "node server.js > /var/log/app.log" {
		t.Errorf("shell form was altered: %q", shell)
	}

	exec := (&startSpec{Entrypoint: []string{"/opt/my app/run"}, Cmd: []string{"--flag"}}).Command()
	if exec != `'/opt/my app/run' --flag` {
		t.Errorf("exec form = %q; a path with a space must stay one argument", exec)
	}
}

// A spec that declares no command is not a broken image -- a Dockerfile may
// inherit its CMD from a base image, which the tar exporter cannot see. It
// must not produce an empty command line that the supervisor then runs.
func TestASpecWithNoCommandIsIgnored(t *testing.T) {
	withSpec(t, `{"workdir": "/app", "from_dockerfile_only": true}`)

	if _, err := applyInit(initRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(appPath); err == nil {
		t.Error("an empty command was written; the app unit would try to run nothing")
	}
}

// The golden template has no build behind it and carries no spec. Its creates
// pass a command explicitly, and a missing file must be silent rather than an
// error -- it is the ordinary case for half the machines on the fleet.
func TestAMissingSpecIsNotAnError(t *testing.T) {
	withSpec(t, "")

	if _, err := applyInit(initRequest{AppCmd: "explicit"}); err != nil {
		t.Fatalf("a machine with no build spec failed to initialise: %v", err)
	}
	cmd, _, err := readAppCmd()
	if err != nil || cmd != "explicit" {
		t.Errorf("command = %q, err = %v", cmd, err)
	}
}

// quoteEnvValue and unquoteEnvValue must be exact inverses.
//
// pilot-app.service reads these files through systemd's EnvironmentFile
// parser and the supervisor reads them through unquoteEnvValue. If the two
// disagree, one machine runs a different command from another for no reason
// the operator can see, decided by whether its image happened to carry
// systemd.
func TestEnvValuesRoundTrip(t *testing.T) {
	for _, v := range []string{
		"plain",
		"with spaces",
		`has "quotes"`,
		`back\slash`,
		"trailing space ",
		"node server.js > /var/log/app.log",
		`{"json":"value"}`,
		"",
	} {
		if got := unquoteEnvValue(quoteEnvValue(v)); got != v {
			t.Errorf("round trip of %q gave %q", v, got)
		}
	}
}
