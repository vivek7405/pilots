package machines

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A unit of work that is slow but inside its budget keeps the loop ticking.
//
// This is the production failure in miniature: one idle suspend uploads a
// memory image, that took longer than the idle monitor's whole liveness
// budget, and the watchdog restarted hostd mid-upload, forever. Remove the
// beat in vouchWhile and ticks stays at zero.
func TestASlowUnitInsideItsBudgetIsVouchedFor(t *testing.T) {
	var ticks atomic.Int64
	err := vouchWhile(context.Background(), time.Second, 5*time.Millisecond,
		func() { ticks.Add(1) },
		func(context.Context) error { time.Sleep(60 * time.Millisecond); return nil })
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	if n := ticks.Load(); n < 3 {
		t.Fatalf("a 60ms unit was vouched for %d times at 5ms; the watchdog would "+
			"have read it as a wedged loop", n)
	}
}

// Past its budget the unit is cancelled AND no longer vouched for, so one that
// ignores its context is still caught by the watchdog. Vouching forever would
// trade a false restart for a wedge nothing ever notices.
func TestAUnitPastItsBudgetIsCancelledAndNoLongerVouchedFor(t *testing.T) {
	var ticks atomic.Int64
	release := make(chan struct{})
	cancelled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- vouchWhile(context.Background(), 20*time.Millisecond, 5*time.Millisecond,
			func() { ticks.Add(1) },
			func(ctx context.Context) error {
				<-ctx.Done()
				close(cancelled)
				<-release // a unit that ignores its cancellation
				return ctx.Err()
			})
	}()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the unit was never cancelled at its budget")
	}
	at := ticks.Load()
	time.Sleep(60 * time.Millisecond)
	if now := ticks.Load(); now != at {
		t.Fatalf("still vouching %d ticks after the budget passed", now-at)
	}
	close(release)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the unit's own deadline error", err)
	}
}

// The unit's result is the caller's result: errBusy still means "leave it".
func TestTheUnitsErrorIsReturnedUnchanged(t *testing.T) {
	err := vouchWhile(context.Background(), time.Second, time.Millisecond, func() {},
		func(context.Context) error { return errBusy })
	if !errors.Is(err, errBusy) {
		t.Fatalf("got %v, want errBusy", err)
	}
}
