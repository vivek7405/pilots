// Package services turns a release into running machines, and only lets a
// route point at them once they have proved they serve.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// HealthSpec is how a service proves a replica is serving.
//
// A tagged union rather than an HTTP path, because a database ships a command
// check and not an endpoint, and every stock image already declares one. The
// shape is Docker's, taken from uncloud's HealthcheckSpec (pkg/api/service.go)
// so a stock image's own HEALTHCHECK maps straight in:
//
//	{"type":"http","path":"/__webjs/ready","interval":15,"timeout":3,"grace":40,"healthy_threshold":2}
//	{"type":"cmd","test":["CMD-SHELL","pg_isready -U postgres"],"interval":15,"timeout":3,"grace":40,"retries":5}
//	{"type":"process","interval":15,"timeout":3,"grace":40,"healthy_threshold":2}
//
// "process" is the check for a service that declared none and has no HTTP
// port to probe -- a private database from a stock image. It is Docker's own
// semantics for a container with no HEALTHCHECK: started, and staying up. The
// supervised app process has to be running and must not have restarted
// between consecutive checks, so an entrypoint that dies and is restarted
// every few seconds (initdb refusing its data directory, say) fails the gate
// rather than being reported as deployed.
type HealthSpec struct {
	Type string `json:"type"` // "http" | "cmd" | "process" | "none"

	// Path is the HTTP check's request path.
	Path string `json:"path,omitempty"`
	// Test is the command check, in Docker's form: ["CMD-SHELL", "..."] runs
	// through a shell, ["CMD", "argv0", ...] does not, ["NONE"] disables.
	Test []string `json:"test,omitempty"`

	IntervalSec int `json:"interval,omitempty"`
	TimeoutSec  int `json:"timeout,omitempty"`
	// GraceSec is how long a replica may take to become healthy before the
	// deploy gives up on it. Docker calls this start_period.
	GraceSec int `json:"grace,omitempty"`
	// HealthyThreshold is how many consecutive successes count as healthy. One
	// success can be a server that is up but still warming; the webjs
	// readiness endpoint answers 503 while its analysis warms and the probe
	// itself drives the warm, so polling is the mechanism rather than just
	// observation.
	HealthyThreshold int `json:"healthy_threshold,omitempty"`
}

// Defaults fills the zeroes. The grace default matches the webjs scaffold's
// own HEALTHCHECK start-period, which is the readiness contract this platform
// is built to serve.
func (h HealthSpec) Defaults() HealthSpec {
	if h.Type == "" {
		h.Type = "http"
	}
	if h.Path == "" {
		h.Path = "/"
	}
	if h.IntervalSec == 0 {
		h.IntervalSec = 15
	}
	if h.TimeoutSec == 0 {
		h.TimeoutSec = 3
	}
	if h.GraceSec == 0 {
		h.GraceSec = 40
	}
	if h.HealthyThreshold == 0 {
		h.HealthyThreshold = 2
	}
	return h
}

// ParseHealth reads the spec off a service row. An empty column is the default
// HTTP check rather than an error: a service that named none still has to be
// gated, or "deployed" would mean "booted".
func ParseHealth(raw string) (HealthSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return HealthSpec{}.Defaults(), nil
	}
	var h HealthSpec
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return HealthSpec{}, fmt.Errorf("services: health spec: %w", err)
	}
	return h.Defaults(), nil
}

// Disabled reports a check that opts out. Docker spells it ["NONE"].
func (h HealthSpec) Disabled() bool {
	return h.Type == "none" || (len(h.Test) == 1 && h.Test[0] == "NONE")
}

// probe runs the check once and reports why it failed, verbatim.
//
// The body matters and is deliberately returned rather than collapsed: the
// webjs readiness endpoint answers 503 with {"status":"error","error":…}
// carrying the actual analysis failure, and a deploy log that says "not
// healthy" instead of that error has thrown away the only useful thing it had.
func (m *Manager) probe(ctx context.Context, machineID string, h HealthSpec) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(h.TimeoutSec)*time.Second)
	defer cancel()

	switch h.Type {
	case "cmd":
		return m.probeCmd(ctx, machineID, h)
	case "process":
		return m.probeProcess(ctx, machineID)
	default:
		return m.probeHTTP(ctx, machineID, h)
	}
}

// agentProcess is the part of the guest agent's process status the gate
// reads. The agent's own type is the contract; this is a projection of it.
type agentProcess struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Restarts int    `json:"restarts"`
	Port     bool   `json:"port"`
}

// probeProcess passes when the machine's app process is running and has not
// restarted since the previous probe. The first probe of a machine only
// establishes the baseline it is running at; a restart is only visible
// between two probes, which is why the check needs HealthyThreshold > 1 to
// mean anything and why Defaults gives it 2.
func (m *Manager) probeProcess(ctx context.Context, machineID string) error {
	raw, err := m.opts.Machines.Processes(ctx, machineID)
	if err != nil {
		return &probeFailure{api.HealthLast{Error: "the replica's agent could not list its processes: " + err.Error()}}
	}
	// The agent answers {"processes":[...]} -- see handleProcesses in
	// cmd/guest-agent. Decoded through that envelope, not as a bare list: the
	// first cut of this decoded a list, its fake agreed with it, and the real
	// agent's answer was "unreadable" on every probe.
	var listing struct {
		Processes []agentProcess `json:"processes"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return &probeFailure{api.HealthLast{Error: "the replica's process list was unreadable: " + err.Error()}}
	}
	app, ok := appProcess(listing.Processes)
	if !ok {
		return &probeFailure{api.HealthLast{Error: "the replica supervises no app process"}}
	}
	if app.State != "running" {
		return &probeFailure{api.HealthLast{Error: fmt.Sprintf(
			"the app process is %s after %d restarts: read the replica's console", app.State, app.Restarts)}}
	}
	if last, seen := m.restartsSeen.Swap(machineID, app.Restarts); seen && app.Restarts != last.(int) {
		return &probeFailure{api.HealthLast{Error: fmt.Sprintf(
			"the app process exited and was restarted (%d restarts so far): read the replica's console",
			app.Restarts)}}
	}
	return nil
}

// appProcess picks the process the gate judges: the one holding the app port,
// else the one named for the image's own command, else the only one there is.
func appProcess(procs []agentProcess) (agentProcess, bool) {
	for _, p := range procs {
		if p.Port {
			return p, true
		}
	}
	for _, p := range procs {
		if p.Name == "app" {
			return p, true
		}
	}
	if len(procs) == 1 {
		return procs[0], true
	}
	return agentProcess{}, false
}

// probeFailure is one failed probe with no address in it.
//
// The probe target is this host's view of the replica, a 10.x address inside
// a network namespace. Printing it sent people to debug a host they cannot
// reach from where they are reading the error, so it is not in the answer at
// all; what is in the answer is what the replica said, or why it said nothing.
type probeFailure struct{ last api.HealthLast }

func (p *probeFailure) Error() string {
	if p.last.Error != "" {
		return p.last.Error
	}
	return fmt.Sprintf("last answer was %d %s", p.last.Status, p.last.Body)
}

func (m *Manager) probeHTTP(ctx context.Context, machineID string, h HealthSpec) error {
	addr, ok := m.opts.Machines.AppAddr(machineID)
	if !ok {
		return &probeFailure{api.HealthLast{Error: "the replica has no address yet"}}
	}
	url := "http://" + addr + h.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return &probeFailure{api.HealthLast{Error: describeDial(err, addr)}}
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return &probeFailure{api.HealthLast{
			Status: resp.StatusCode, Body: readBody(resp)}}
	}
	return nil
}

// describeDial names the failure without the address. Connection refused is
// the one that matters and it has exactly two causes worth naming: an app on
// the wrong port, or an app bound to 127.0.0.1 inside the guest.
func describeDial(err error, addr string) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused on port 8080: the app is not listening on 0.0.0.0:$PORT"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
		return "no answer on port 8080 within the timeout"
	}
	return strings.ReplaceAll(err.Error(), addr, "the replica")
}

func (m *Manager) probeCmd(ctx context.Context, machineID string, h HealthSpec) error {
	cmd, err := commandOf(h.Test)
	if err != nil {
		return err
	}
	// Through the guest agent's exec, which is where a command check has to
	// run: the check is about the process's own view, not the host's.
	res, err := m.opts.Machines.Exec(ctx, machineID, api.ExecRequest{Cmd: cmd, User: "root"})
	if err != nil {
		return &probeFailure{api.HealthLast{
			Error: fmt.Sprintf("exec %q: %v", cmd, err)}}
	}
	if res.ExitCode != 0 {
		return &probeFailure{api.HealthLast{
			Error: fmt.Sprintf("exec %q exited %d: %s", cmd, res.ExitCode,
				strings.TrimSpace(res.Stdout+res.Stderr))}}
	}
	return nil
}

// commandOf renders Docker's Test form as one shell command.
func commandOf(test []string) (string, error) {
	if len(test) == 0 {
		return "", fmt.Errorf("services: cmd health check has no test")
	}
	switch test[0] {
	case "CMD-SHELL":
		return strings.Join(test[1:], " "), nil
	case "CMD":
		return shellJoin(test[1:]), nil
	case "NONE":
		return "", fmt.Errorf("services: health check is disabled")
	default:
		// No prefix: Docker treats a bare list as CMD.
		return shellJoin(test), nil
	}
}

func shellJoin(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`&|;<>()*?[]{}~#!") {
			quoted = append(quoted, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
			continue
		}
		quoted = append(quoted, a)
	}
	return strings.Join(quoted, " ")
}

func readBody(resp *http.Response) string {
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

// waitHealthy polls until the replica passes HealthyThreshold consecutive
// checks, or the grace period runs out.
//
// The last failure is what the caller reports, because "did not become
// healthy in 40s" is not actionable and "503 {"status":"error", …}" is.
func (m *Manager) waitHealthy(ctx context.Context, machineID string, h HealthSpec) error {
	if h.Disabled() {
		return nil
	}
	// The process check compares consecutive probes; a gate starts with no
	// history, and leaves none behind for the next rollout of this machine.
	m.restartsSeen.Delete(machineID)
	defer m.restartsSeen.Delete(machineID)
	deadline := time.Now().Add(time.Duration(h.GraceSec) * time.Second)
	interval := time.Duration(h.IntervalSec) * time.Second

	// Poll faster than the steady-state interval while gating: the interval is
	// tuned for watching a healthy service, and using it here would add up to
	// interval seconds of pure waiting to every deploy.
	if probe := interval / 5; probe > 0 && probe < interval {
		interval = probe
	}
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}

	var consecutive int
	var last error
	for {
		if err := m.probe(ctx, machineID, h); err != nil {
			consecutive, last = 0, err
		} else {
			consecutive++
			if consecutive >= h.HealthyThreshold {
				return nil
			}
		}
		if time.Now().After(deadline) {
			// Typed, not a sentence: the handler turns this into the 422 and
			// the SDKs branch on it, so the replica, the grace and the last
			// answer have to survive as fields rather than as prose.
			return &api.HealthGateDetails{
				Replica: machineID, GraceSec: h.GraceSec,
				Last: lastOf(last, consecutive, h.HealthyThreshold),
			}
		}
		select {
		case <-ctx.Done():
			// Named, not passed through bare. This is overwhelmingly the
			// caller hanging up -- a client deadline, a Ctrl-C -- and a bare
			// "context canceled" in a deploy's error says neither which
			// replica was being gated nor what it was answering, which is the
			// one thing anybody needs. The last probe failure is the cause;
			// the cancellation is only when we stopped waiting for it.
			if last != nil {
				return fmt.Errorf("machine %s was still not healthy when the "+
					"deploy stopped waiting (%w); its last answer was: %v",
					machineID, ctx.Err(), last)
			}
			return fmt.Errorf("machine %s: the deploy stopped waiting for it: %w",
				machineID, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// lastOf reads the replica's last answer off the last probe failure. A nil
// last means every probe passed but never enough of them in a row, which is
// its own answer and not a failure to report.
func lastOf(last error, consecutive, threshold int) api.HealthLast {
	var p *probeFailure
	if errors.As(last, &p) {
		return p.last
	}
	if last != nil {
		return api.HealthLast{Error: last.Error()}
	}
	return api.HealthLast{Error: fmt.Sprintf(
		"only %d of %d consecutive checks passed", consecutive, threshold)}
}

// withRelease names the service and release on a gate failure; any other
// error passes through untouched. One helper because six call sites in
// manager.go would otherwise each carry their own copy of the same two lines.
func withRelease(err error, serviceID, releaseID string) error {
	var gate *api.HealthGateDetails
	if errors.As(err, &gate) {
		gate.Service, gate.Release = serviceID, releaseID
	}
	return err
}
