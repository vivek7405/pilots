package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// headerProxyPort routes a request to an arbitrary port inside the guest.
const headerProxyPort = "X-Pilot-Proxy-Port"

type initRequest struct {
	TimestampNanos int64 `json:"timestamp_nanos"`

	// Env is the application's environment. Written to a file the app unit
	// reads, never passed as arguments -- a process's argv is world-readable
	// on /proc and a secret does not belong there.
	Env map[string]string `json:"env,omitempty"`
	// AppCmd is what this machine runs, as a shell command. Absent means the
	// image already carries one.
	AppCmd string `json:"app_cmd,omitempty"`
	// StartApp asks the agent to start the application.
	//
	// Sent on a create and on NOTHING else. A wake resumes a snapshot in which
	// the application is already running, and starting it there would restart
	// the very process the guest just restored -- losing whatever it held in
	// memory, in exchange for an environment change that a wake is not
	// supposed to deliver anyway.
	StartApp bool `json:"start_app,omitempty"`
}

type initResponse struct {
	OK bool `json:"ok"`
	// AppStarted and AppReason are how the host learns that a start did not
	// happen. Silence would let the create path believe it delivered an
	// environment to a process that never read one.
	AppStarted bool   `json:"app_started"`
	AppReason  string `json:"app_reason,omitempty"`
}

// envPath holds the application's environment, in the format the app unit
// reads with EnvironmentFile. appPath holds the command, separately, so a
// redeploy that changes only the environment does not rewrite it.
//
// Variables rather than constants so the tests can write somewhere that is not
// the real /etc.
var (
	envPath = "/etc/pilot/env"
	appPath = "/etc/pilot/app"
)

const (
	// appUnit is started by the agent rather than enabled in the image. That
	// is the golden template contract: the template settles the base system
	// and stops, because a create is a resume and there is no moment at which
	// a resumed process can be handed an environment it did not start with.
	appUnit = "pilot-app.service"
)

// handleInit sets the guest's wall clock.
//
// A restored machine resumes with CLOCK_REALTIME frozen at the instant the
// snapshot was taken, which can be hours or days stale. The visible symptom is
// nasty and non-obvious: the guest accepts TCP connections at the kernel layer
// but nothing ever reads them, and TLS handshakes fail on certificate validity
// windows. hostd calls this immediately after every resume.
//
// Only CLOCK_REALTIME needs setting -- kvm-clock keeps CLOCK_MONOTONIC honest
// across a snapshot.
func handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}
	if req.TimestampNanos <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "timestamp_nanos is required"})
		return
	}

	ts := unix.NsecToTimespec(req.TimestampNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		log.Printf("guest-agent: clock_settime: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp, err := applyInit(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyInit is everything a CREATE asks for, and everything a wake does not.
//
// Split out from the clock correction above because the clock correction is
// the only part a wake wants: hostd pokes /init after every resume to unfreeze
// CLOCK_REALTIME, and that poke carries a timestamp and nothing else. A wake
// therefore arrives here with a nil environment, no command and StartApp
// false, and every branch below is skipped -- which is the asymmetry the whole
// design turns on, and is what this shape makes testable without a microVM.
func applyInit(req initRequest) (initResponse, error) {
	// An explicit command wins; the build's declared spec is the fallback.
	//
	// Without the fallback a machine created from a user's Dockerfile has a
	// start command sitting in /etc/pilot-agent/start.json that nothing reads,
	// comes up, answers health checks, reports app_started, and runs no
	// application. See startspec.go.
	cmd, env := req.AppCmd, req.Env
	if cmd == "" {
		if spec, ok := readStartSpec(); ok {
			cmd = spec.Command()
			// The Dockerfile's ENV is the application's baseline. What the
			// create supplied wins on a collision: a deploy-time value is
			// meant to override what the image was built with.
			if len(spec.Env) > 0 {
				merged := make(map[string]string, len(spec.Env)+len(env))
				for k, v := range spec.Env {
					merged[k] = v
				}
				for k, v := range env {
					merged[k] = v
				}
				env = merged
			}
			if spec.WorkDir != "" {
				appWorkDir = spec.WorkDir
			}
			if spec.User != "" {
				appUser = spec.User
			}
		}
	}
	if err := writeEnv(env); err != nil {
		return initResponse{}, err
	}
	if cmd != "" {
		if err := writeAppCmd(cmd); err != nil {
			return initResponse{}, err
		}
	}

	resp := initResponse{OK: true}
	if req.StartApp {
		resp.AppStarted, resp.AppReason = startApp()
	}
	return resp, nil
}

// writeEnv renders the application's environment where the app unit reads it.
//
// A nil map is not the same as an empty one: nil means the caller said nothing
// about the environment, and rewriting the file with nothing in it would strip
// an application of the environment it is already running with.
func writeEnv(env map[string]string) error {
	if env == nil {
		return nil
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		if !validEnvName(k) {
			return fmt.Errorf("environment name %q is not usable", k)
		}
		keys = append(keys, k)
	}
	// Sorted so the file is byte-identical for the same environment, which is
	// what makes "did the environment change" answerable by looking at it.
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, quoteEnvValue(env[k]))
	}

	if err := os.MkdirAll(filepath.Dir(envPath), 0o700); err != nil {
		return err
	}
	// 0600: this file holds every secret the machine was deployed with, and
	// the workload runs as a real user with a shell.
	return os.WriteFile(envPath, []byte(b.String()), 0o600)
}

// quoteEnvValue renders a value the way systemd's EnvironmentFile parser reads
// it back: double quoted, with backslashes, quotes and newlines escaped.
//
// Unquoted values look fine until one contains a space or a '#', at which
// point the application receives a truncated secret and fails somewhere else
// entirely.
func quoteEnvValue(v string) string {
	// Only the backslash and the quote. A newline is written as ITSELF,
	// inside the quotes, because that is the one form systemd understands:
	// its EnvironmentFile parser takes the byte after a backslash literally
	// and has no \n escape, so "one\ntwo" reached the application as the
	// eleven characters o n e \ n t w o. A PEM key delivered through
	// secret_env arrived unusable -- but only on images carrying systemd,
	// since the supervisor's own reader did understand \n. Same machine
	// definition, two behaviours, decided by whether the image happened to
	// ship an init.
	//
	// A quoted value may span lines, which is why the reader parses rather
	// than splits.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// validEnvName rejects names that would produce a file systemd reads as
// something else.
func validEnvName(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for _, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// writeAppCmd records what this machine runs.
func writeAppCmd(cmd string) error {
	if strings.ContainsAny(cmd, "\n\r") {
		return fmt.Errorf("the application command may not span lines")
	}
	if err := os.MkdirAll(filepath.Dir(appPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(appPath, []byte("PILOT_APP_CMD="+quoteEnvValue(cmd)+"\n"), 0o644)
}

// startApp starts the application unit, and refuses to restart one that is
// already running.
//
// The refusal is the point. A create is the only time the application should
// be started, and the failure it guards against -- a wake that re-execs the
// app -- looks like success from the outside: the machine is up and serving,
// and the process the guest spent its snapshot restoring is gone. So it is
// reported rather than silently tolerated, and the host logs it.
// Variables rather than functions so a test can observe whether a start was
// even attempted. "Was startApp called at all" is the question the wake path
// turns on, and it cannot be answered by looking at the result.
var (
	startApp = func() (bool, string) {
		cmdline, env, err := readAppCmd()
		if err != nil || cmdline == "" {
			return false, "this image carries no application command"
		}

		// Which mechanism, decided by looking rather than by assuming. The
		// golden template carries systemd and its unit; an image built from a
		// user's Dockerfile carries neither, and `systemctl` there is not a
		// failure to handle but a command that does not exist. See
		// supervise.go.
		if !systemdIsRunning() {
			return appSupervisor.start(cmdline, env)
		}
		if run("systemctl", "is-active", "--quiet", appUnit) == nil {
			return false, "the application is already running; not restarting it"
		}
		if err := run("systemctl", "start", appUnit); err != nil {
			return false, "systemctl start " + appUnit + ": " + err.Error()
		}
		return true, ""
	}

	run = func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil && len(out) > 0 {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return err
	}
)

// proxyToLocalPort forwards a request to 127.0.0.1:<port> inside the guest.
//
// The original Host header is preserved so the application sees the URL the
// end user typed, and the routing header is stripped so it cannot be
// reflected onward or confuse the application.
func proxyToLocalPort(w http.ResponseWriter, r *http.Request, port string) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Header.Del(headerProxyPort)
		// Host stays as the client sent it: applications generate absolute
		// URLs and set cookies from it.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// The application is not listening yet, or has crashed. 502 is
		// accurate and lets the router distinguish this from the machine
		// itself being down.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "no application listening on port " + port,
		})
	}
	proxy.ServeHTTP(w, r)
}
