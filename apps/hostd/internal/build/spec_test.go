package build

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Exec form is a JSON array Docker exec's directly; shell form is a command
// line it runs through /bin/sh -c. The difference is not cosmetic at the
// moment of exec -- running a shell-form command as argv tries to exec a
// program named after the whole command line, and no redirection or variable
// expands.
func TestParseStartSpecTellsExecFormFromShellForm(t *testing.T) {
	exec := ParseStartSpec("FROM alpine\nCMD [\"node\", \"server.js\"]\n")
	if !reflect.DeepEqual(exec.Cmd, []string{"node", "server.js"}) {
		t.Fatalf("exec form parsed as %#v", exec.Cmd)
	}
	if exec.Shell {
		t.Error("exec form was marked as shell form")
	}

	shell := ParseStartSpec("FROM alpine\nCMD npm start\n")
	if !reflect.DeepEqual(shell.Cmd, []string{"npm start"}) {
		t.Fatalf("shell form parsed as %#v", shell.Cmd)
	}
	if !shell.Shell {
		t.Error("shell form was not marked; it would be exec'd as a single argv")
	}
}

// A malformed JSON array is not quietly reinterpreted as shell form, which
// would run something subtly different from what was written.
func TestParseStartSpecRefusesAMalformedExecForm(t *testing.T) {
	got := ParseStartSpec("FROM alpine\nCMD [\"node\", \"server.js\"\n")
	if len(got.Cmd) != 0 {
		t.Fatalf("a broken array became %#v", got.Cmd)
	}
}

// The FINAL stage only. A multi-stage build's earlier stages describe a
// toolchain that is thrown away, and taking a builder stage's CMD would start
// the compiler instead of the application.
func TestParseStartSpecUsesTheFinalStage(t *testing.T) {
	got := ParseStartSpec(`
FROM golang:1.23 AS builder
WORKDIR /src
ENV CGO_ENABLED=0
CMD ["go", "build", "./..."]
EXPOSE 9999

FROM alpine:3.20
WORKDIR /app
ENV NODE_ENV=production
EXPOSE 8080
CMD ["/app/server"]
`)
	if !reflect.DeepEqual(got.Cmd, []string{"/app/server"}) {
		t.Fatalf("cmd is %#v; a builder stage's command would start the compiler", got.Cmd)
	}
	if got.WorkDir != "/app" {
		t.Errorf("workdir is %q, want /app", got.WorkDir)
	}
	if got.Port != 8080 {
		t.Errorf("port is %d, want 8080", got.Port)
	}
	if _, leaked := got.Env["CGO_ENABLED"]; leaked {
		t.Errorf("the builder stage's env leaked into the final stage: %v", got.Env)
	}
	if got.Env["NODE_ENV"] != "production" {
		t.Errorf("env is %v", got.Env)
	}
}

func TestParseStartSpecHandlesContinuationsAndComments(t *testing.T) {
	got := ParseStartSpec(`FROM alpine
# the entrypoint, split over lines the way a real Dockerfile does it
ENTRYPOINT ["/usr/bin/tini", \
            "--", \
            "/app/start.sh"]
CMD ["--serve"]
`)
	want := []string{"/usr/bin/tini", "--", "/app/start.sh"}
	if !reflect.DeepEqual(got.Entrypoint, want) {
		t.Fatalf("entrypoint is %#v, want %#v", got.Entrypoint, want)
	}
	if !reflect.DeepEqual(got.Cmd, []string{"--serve"}) {
		t.Fatalf("cmd is %#v", got.Cmd)
	}
}

func TestParseStartSpecEnvForms(t *testing.T) {
	got := ParseStartSpec(`FROM alpine
ENV PORT=3000 GREETING="hello world"
ENV LEGACY_FORM some value here
USER app
`)
	if got.Env["PORT"] != "3000" {
		t.Errorf("PORT is %q", got.Env["PORT"])
	}
	if got.Env["GREETING"] != "hello world" {
		t.Errorf("a quoted value with a space was split: %q", got.Env["GREETING"])
	}
	if got.Env["LEGACY_FORM"] != "some value here" {
		t.Errorf("the legacy ENV form is %q", got.Env["LEGACY_FORM"])
	}
	if got.User != "app" {
		t.Errorf("user is %q", got.User)
	}
}

func TestParseStartSpecExposeSuffix(t *testing.T) {
	got := ParseStartSpec("FROM alpine\nEXPOSE 8080/tcp 9090\n")
	if got.Port != 8080 {
		t.Fatalf("port is %d, want 8080", got.Port)
	}
}

// The honest limit, and the reason it is written into the file rather than
// left to be discovered. A Dockerfile that inherits its command from its base
// image yields an empty spec, and a consumer has to be able to tell that from
// "this application declares no start command".
func TestStartSpecRecordsThatItSawOnlyTheDockerfile(t *testing.T) {
	got := ParseStartSpec("FROM node:24-alpine\nCOPY . /app\n")
	if !got.Empty() {
		t.Fatalf("expected an empty spec, got %#v", got)
	}

	raw, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["from_dockerfile_only"] != true {
		t.Fatalf("the spec does not record where it came from: %s", raw)
	}
	// Absent rather than present-and-empty, so a consumer's own "is this set"
	// check works.
	if _, ok := decoded["cmd"]; ok {
		t.Errorf("an undeclared cmd was serialised anyway: %s", raw)
	}
}

// Instructions are case-insensitive in a Dockerfile.
func TestParseStartSpecAcceptsLowercaseInstructions(t *testing.T) {
	got := ParseStartSpec("from alpine\ncmd [\"/bin/app\"]\n")
	if !reflect.DeepEqual(got.Cmd, []string{"/bin/app"}) {
		t.Fatalf("cmd is %#v", got.Cmd)
	}
}
