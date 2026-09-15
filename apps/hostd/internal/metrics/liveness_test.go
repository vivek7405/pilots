package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestALoopInsideItsBudgetIsNotOverdue(t *testing.T) {
	resetLoops()
	t.Cleanup(resetLoops)

	l := NewLoop("ledger", time.Minute)
	l.Tick()
	if got := Overdue(); len(got) != 0 {
		t.Errorf("Overdue = %v on a freshly ticked loop", got)
	}
}

// The whole point: a loop that has stopped ticking is named, so the watchdog
// can withhold its pet and the journal can say which loop wedged.
func TestALoopPastItsBudgetIsOverdueAndNamed(t *testing.T) {
	resetLoops()
	t.Cleanup(resetLoops)

	NewLoop("ledger", time.Minute)
	NewLoop("self_heal", time.Minute)

	// Two minutes on, only the loop that never ticked again is overdue.
	later := time.Now().Add(2 * time.Minute)
	got := overdueAt(later)
	if len(got) != 2 {
		t.Fatalf("Overdue = %v, want both loops", got)
	}
	if got[0] != "ledger" || got[1] != "self_heal" {
		t.Errorf("Overdue = %v, want it sorted so two restarts read the same", got)
	}

	loops["ledger"].last = later
	if got := overdueAt(later); len(got) != 1 || got[0] != "self_heal" {
		t.Errorf("Overdue = %v, want only the loop that is still stalled", got)
	}
}

// A loop registered twice keeps its clock. A test that builds two managers
// must not silently reset the production loop's liveness.
func TestRegisteringALoopTwiceKeepsTheFirst(t *testing.T) {
	resetLoops()
	t.Cleanup(resetLoops)

	first := NewLoop("ledger", time.Minute)
	first.Tick()
	stamp := first.last

	second := NewLoop("ledger", time.Hour)
	if second != first {
		t.Fatal("the second registration replaced the first")
	}
	if second.last != stamp {
		t.Error("the second registration reset the loop's clock")
	}
	if second.budget != time.Minute {
		t.Errorf("budget = %v, want the first registration's", second.budget)
	}
}

// The gauge is what an operator alerts on, so it has to render with the loop
// as a label rather than as a name per loop.
func TestTheLoopGaugeRendersWithItsLabel(t *testing.T) {
	resetLoops()
	t.Cleanup(resetLoops)

	NewLoop("usage_ledger", time.Minute).Tick()
	var buf bytes.Buffer
	Default.Render(&buf)
	out := buf.String()
	if !strings.Contains(out, `pilots_loop_last_tick_seconds{loop="usage_ledger"}`) {
		t.Errorf("the scrape does not carry the loop's series:\n%s", firstLines(out, 40))
	}
}

// A nil loop is a no-op, so a caller that has no registry (a test manager, a
// fake) needs no branch at the call site.
func TestANilLoopDoesNotPanic(t *testing.T) {
	var l *Loop
	l.Tick()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
