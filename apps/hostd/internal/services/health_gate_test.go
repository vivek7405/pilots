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
