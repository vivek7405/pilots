package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// A gate that runs out of grace returns the typed failure, not a sentence.
//
// The handler turns it into the 422 and both SDKs branch on it, so if this
// ever goes back to fmt.Errorf the whole contract degrades to prose without a
// single other test going red.
func TestWaitHealthyReturnsTheTypedGateFailure(t *testing.T) {
	m, fm, _, _ := fixture(t, 1)
	fm.healthy["m-1"] = false

	spec := HealthSpec{
		Type: "cmd", Test: []string{"CMD-SHELL", "exit 1"},
		GraceSec: 1, IntervalSec: 1, HealthyThreshold: 1,
	}
	err := withRelease(m.waitHealthy(context.Background(), "m-1", spec), "svc-1", "rel-1")

	var gate *api.HealthGateDetails
	if !errors.As(err, &gate) {
		t.Fatalf("waitHealthy returned %T (%v), want *api.HealthGateDetails", err, err)
	}
	if gate.Replica != "m-1" {
		t.Errorf("replica = %q, want m-1", gate.Replica)
	}
	if gate.Service != "svc-1" || gate.Release != "rel-1" {
		t.Errorf("withRelease did not name the service and release: %+v", gate)
	}
	if gate.GraceSec != 1 {
		t.Errorf("grace_sec = %d, want 1", gate.GraceSec)
	}
	if gate.Last.Error == "" && gate.Last.Status == 0 {
		t.Error("the last answer is empty, so the caller is told nothing actionable")
	}
	if regexp.MustCompile(`\b10\.\d+\.\d+\.\d+\b`).MatchString(gate.Error()) {
		t.Errorf("the gate's text carries an address: %s", gate.Error())
	}
}

// describeDial names the one cause that matters and strips the address from
// everything else, which is the last place the address could still leak.
func TestDescribeDialNamesTheCauseWithoutTheAddress(t *testing.T) {
	const addr = "10.0.42.2:8080"

	refused := fmt.Errorf("dial tcp %s: connect: %w", addr, syscall.ECONNREFUSED)
	got := describeDial(refused, addr)
	if !strings.Contains(got, "0.0.0.0:$PORT") {
		t.Errorf("a refused connection did not name the bind address: %s", got)
	}
	if strings.Contains(got, addr) {
		t.Errorf("the address survived a refused connection: %s", got)
	}

	other := errors.New("dial tcp " + addr + ": some other failure")
	if got := describeDial(other, addr); strings.Contains(got, addr) {
		t.Errorf("the address survived a failure with no named cause: %s", got)
	}
}

// A private service with no check of its own is gated on its process: up, and
// staying up. The fake's unhealthy machine is a crash loop -- every read shows
// one more restart -- which is exactly what an entrypoint that keeps dying and
// being restarted looks like from the host, and it must not pass as deployed.
func TestTheProcessCheckPassesAStableProcessAndFailsACrashLoop(t *testing.T) {
	m, fm, _, _ := fixture(t, 1)
	spec := HealthSpec{Type: "process", GraceSec: 2, IntervalSec: 1, TimeoutSec: 1, HealthyThreshold: 2}

	fm.healthy["m-1"] = true
	if err := m.waitHealthy(context.Background(), "m-1", spec); err != nil {
		t.Fatalf("a process that stayed up was refused: %v", err)
	}

	fm.healthy["m-1"] = false
	err := m.waitHealthy(context.Background(), "m-1", spec)
	var gate *api.HealthGateDetails
	if !errors.As(err, &gate) {
		t.Fatalf("a crash-looping process passed the gate: %v", err)
	}
	if !strings.Contains(gate.Last.Error, "restarted") {
		t.Errorf("the failure does not say the process restarted: %s", gate.Last.Error)
	}
}

// The gate judges the process that holds the app port, or the image's own
// command, never a sidecar somebody registered beside it.
func TestTheProcessCheckJudgesTheAppProcess(t *testing.T) {
	procs := []agentProcess{
		{Name: "worker", State: "running"},
		{Name: "app", State: "stopped", Restarts: 3, Port: true},
	}
	got, ok := appProcess(procs)
	if !ok || got.Name != "app" {
		t.Errorf("picked %+v, want the port holder", got)
	}
	if _, ok := appProcess([]agentProcess{{Name: "a"}, {Name: "b"}}); ok {
		t.Error("two processes and no app: something was picked anyway")
	}
	if got, ok := appProcess([]agentProcess{{Name: "only", State: "running"}}); !ok || got.Name != "only" {
		t.Errorf("a lone process was not taken as the app: %+v", got)
	}
}
