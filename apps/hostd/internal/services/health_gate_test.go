package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/pilotsrun/pilots/hostd/internal/api"
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

// The gate reads the agent's answer as the agent writes it. This document is
// a real GET /processes answer from a postgres replica, verbatim, so a change
// to either side's shape fails here rather than as "unreadable" on a host.
func TestTheProcessCheckReadsTheAgentsOwnEnvelope(t *testing.T) {
	m, fm, _, _ := fixture(t, 1)
	fm.processes = []byte(`{"processes":[{"name":"app","cmd":"docker-entrypoint.sh postgres","state":"running","pid":251,"restarts":0,"port":true}]}`)
	if err := m.probeProcess(context.Background(), "m-1"); err != nil {
		t.Fatalf("the agent's own answer was refused: %v", err)
	}
}

// A replica that failed its gate is kept, suspended, not destroyed: the 422
// tells the caller to read its console and diagnose reads it back, and both
// were lies while the rollout destroyed the machine they named. The next
// deploy that succeeds prunes it like any superseded release's replica.
func TestAGateFailedReplicaIsParkedForDiagnosisThenPruned(t *testing.T) {
	ctx := context.Background()
	m, fm, _, _ := fixture(t, 1)
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-1", nil); err != nil {
		t.Fatal(err)
	}

	fm.events = nil
	fm.createUnhealthy = true
	_, err := m.Deploy(ctx, "svc-1", "rootfs-2", nil)
	var gate *api.HealthGateDetails
	if !errors.As(err, &gate) {
		t.Fatalf("the second deploy did not fail its gate: %v", err)
	}
	var suspended, destroyed bool
	for _, e := range fm.events {
		if e == "suspend:"+gate.Replica {
			suspended = true
		}
		if e == "destroy:"+gate.Replica {
			destroyed = true
		}
	}
	if !suspended || destroyed {
		t.Fatalf("the failed replica %s was not parked (suspended=%v destroyed=%v): %v",
			gate.Replica, suspended, destroyed, fm.events)
	}

	// A second failed deploy keeps ITS replica as the evidence and prunes the
	// first failure's, so a run of failed deploys does not stack parked
	// replicas, each holding a slot and its builds.
	fm.events = nil
	_, err = m.Deploy(ctx, "svc-1", "rootfs-3", nil)
	var second *api.HealthGateDetails
	if !errors.As(err, &second) {
		t.Fatalf("the third deploy did not fail its gate: %v", err)
	}
	var firstPruned, secondParked bool
	for _, e := range fm.events {
		if e == "destroy:"+gate.Replica {
			firstPruned = true
		}
		if e == "suspend:"+second.Replica {
			secondParked = true
		}
		if e == "destroy:m-1" {
			t.Fatalf("the current release's replica was pruned by a failed deploy: %v", fm.events)
		}
	}
	if !firstPruned || !secondParked {
		t.Fatalf("second failure: first parked %s pruned=%v, second %s parked=%v: %v",
			gate.Replica, firstPruned, second.Replica, secondParked, fm.events)
	}

	fm.events = nil
	fm.createUnhealthy = false
	if _, err := m.Deploy(ctx, "svc-1", "rootfs-4", nil); err != nil {
		t.Fatalf("the fourth deploy failed: %v", err)
	}
	pruned := false
	for _, e := range fm.events {
		if e == "destroy:"+second.Replica {
			pruned = true
		}
	}
	if !pruned {
		t.Errorf("the parked replica %s survived the next successful deploy: %v", second.Replica, fm.events)
	}
}
