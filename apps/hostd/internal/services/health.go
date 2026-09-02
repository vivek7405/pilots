// Package services turns a release into running machines, and only lets a
// route point at them once they have proved they serve.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
type HealthSpec struct {
	Type string `json:"type"` // "http" | "cmd" | "none"

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
	default:
		return m.probeHTTP(ctx, machineID, h)
	}
}

func (m *Manager) probeHTTP(ctx context.Context, machineID string, h HealthSpec) error {
	addr, ok := m.opts.Machines.AppAddr(machineID)
	if !ok {
		return fmt.Errorf("machine %s has no address yet", machineID)
	}
	url := "http://" + addr + h.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, readBody(resp))
	}
	return nil
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
		return fmt.Errorf("exec %q: %w", cmd, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exec %q exited %d: %s", cmd, res.ExitCode,
			strings.TrimSpace(res.Stdout+res.Stderr))
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
			if last == nil {
				last = fmt.Errorf("only %d of %d consecutive checks passed",
					consecutive, h.HealthyThreshold)
			}
			return fmt.Errorf("machine %s did not become healthy within %ds: %w",
				machineID, h.GraceSec, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
