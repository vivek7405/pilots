package main

import (
	"bytes"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// The counters are read off a table that is deleted and rebuilt whenever the
// fleet moves, so every count returns to zero on a rebuild. Reading a reset as
// traffic would touch every replica's row on the tick after any machine was
// created, which is exactly the signal the autoscaler trusts to mean "busy".
func TestARisingCountIsActivityAndAResetIsNot(t *testing.T) {
	seen := map[string]uint64{}

	if got := risen(seen, map[string]uint64{"m-1": 5}); len(got) != 0 {
		t.Errorf("a first reading reported %v; it only sets the baseline", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 5}); len(got) != 0 {
		t.Errorf("an unchanged count reported %v", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 9}); !reflect.DeepEqual(got, []string{"m-1"}) {
		t.Errorf("a rising count reported %v, want [m-1]", got)
	}
	// The table was rebuilt: the counter is back to zero and nothing arrived.
	if got := risen(seen, map[string]uint64{"m-1": 0}); len(got) != 0 {
		t.Errorf("a rebuilt table reported %v as activity", got)
	}
	if got := risen(seen, map[string]uint64{"m-1": 2}); !reflect.DeepEqual(got, []string{"m-1"}) {
		t.Errorf("traffic after a rebuild reported %v, want [m-1]", got)
	}

	// A machine with no rule any more is forgotten, so a slot reused by a
	// different machine cannot inherit its baseline.
	if got := risen(seen, map[string]uint64{}); len(got) != 0 {
		t.Errorf("an empty reading reported %v", got)
	}
	if _, still := seen["m-1"]; still {
		t.Error("a machine with no rule kept its baseline")
	}
}

// The held-session signal is what stops a replica being suspended out from
// under an open, silent Postgres session. When conntrack cannot be read there
// IS no reading, and reporting zero would make a database under load look
// idle -- the exact mid-transaction kill the signal exists to prevent.
func TestAnUnreadableSessionTableNeverReadsAsNoSessions(t *testing.T) {
	g := newGuestLoad()

	// Before the first pass lands, nothing has been read yet.
	if got := g.Held("m-1"); got == 0 {
		t.Error("a replica read as idle before the first pass had run")
	}

	g.set(map[string]int{"m-1": 3})
	if got := g.Held("m-1"); got != 3 {
		t.Errorf("Held = %d, want the 3 sessions that were read", got)
	}
	if got := g.Held("m-2"); got != 0 {
		t.Errorf("a machine with no sessions read as %d, want 0", got)
	}

	g.unreadable()
	if got := g.Held("m-2"); got == 0 {
		t.Error("a failed read reported no sessions, which is what suspends a live one")
	}
}

// The old reading is dropped, not kept. Keeping it sticks in whichever
// direction the last good pass landed: one that saw sessions holds the
// replica up forever, one that saw none lets it be suspended forever. Neither
// is a reading.
func TestAFailedReadDropsTheOldReadingRatherThanKeepingIt(t *testing.T) {
	g := newGuestLoad()
	g.set(map[string]int{"m-1": 7})
	g.unreadable()

	if got := g.Held("m-1"); got != 1 {
		t.Errorf("Held = %d after the read failed, want the blind answer 1, not the stale 7", got)
	}

	// And a later successful pass is believed immediately, including one that
	// finds nothing -- or a host that failed once could never scale down.
	g.set(map[string]int{})
	if got := g.Held("m-1"); got != 0 {
		t.Errorf("Held = %d after a good pass found nothing, want 0", got)
	}
}

// A repeating failure has to stay loud without burying the log: the loop
// ticks every couple of seconds, so warning on every pass buries the log and
// warning once buries the failure. Warn on the way in, periodically while it
// lasts, and once on the way out.
func TestARepeatingFailureWarnsOnEntryPeriodicallyAndOnRecovery(t *testing.T) {
	var log bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	lines := func() int { return strings.Count(log.String(), "level=WARN") }

	var g errGate
	g.failed("cannot read", errors.New("injected"))
	if got := lines(); got != 1 {
		t.Fatalf("the first failure produced %d warnings, want 1", got)
	}

	for range warnEvery - 1 {
		g.failed("cannot read", errors.New("injected"))
	}
	if got := lines(); got != 2 {
		t.Errorf("%d warnings over %d consecutive failures, want 2: one on entry, "+
			"one periodic -- a failure that stops being mentioned reads as fixed",
			got, warnEvery)
	}

	g.recovered()
	if got := lines(); got != 3 {
		t.Errorf("%d warnings after recovery, want 3: the recovery is worth a line too", got)
	}
	if g.failing || g.n != 0 {
		t.Error("the gate stayed failing after recovery, so the next outage would be silent")
	}
}

// Every failed read is counted, so a host whose conntrack module never loaded
// is findable on a scrape rather than only in a log nobody is tailing.
func TestEveryFailedReadIsCounted(t *testing.T) {
	before := metrics.ActivityReadErrors.Load()
	var g errGate
	g.failed("test", errors.New("injected"))
	g.failed("test", errors.New("injected"))
	if got := metrics.ActivityReadErrors.Load() - before; got != 2 {
		t.Errorf("counted %d failed reads, want 2", got)
	}
}

// And the blindness itself is a gauge, because its consequence is specific:
// while it is 1 nothing on this host is given back, so a flat replica count
// and a rising bill have one thing to check first.
func TestBlindnessIsVisibleOnAScrape(t *testing.T) {
	g := newGuestLoad()
	if got := metrics.SessionSignalBlind.Load(); got != 1 {
		t.Errorf("gauge = %d before the first read, want 1", got)
	}
	g.set(map[string]int{})
	if got := metrics.SessionSignalBlind.Load(); got != 0 {
		t.Errorf("gauge = %d after a good read, want 0", got)
	}
	g.unreadable()
	if got := metrics.SessionSignalBlind.Load(); got != 1 {
		t.Errorf("gauge = %d after a failed read, want 1", got)
	}
}
