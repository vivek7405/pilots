package metrics

import (
	"context"
	"time"
)

// VouchWhile runs one unit of a monitored loop's work and keeps the loop's
// liveness tick going while that unit is inside its own budget.
//
// A tick after each unit was not enough. The unit itself, one idle suspend,
// outran the idle monitor's 30s budget as soon as the bucket was a real one:
// the watchdog killed hostd in the middle of the upload, the restarted hostd
// found the same machine still idle, and suspended it again. The host restarted every fifty
// seconds for as long as any machine on it was idle, which on a platform built
// on scale-to-zero is always. Local object storage hid it on every rig.
//
// The unit gets a deadline of its own instead. Inside it the loop is alive and
// says so; past it the context is cancelled, which is what unblocks a stuck
// upload, and the ticking STOPS, so a unit that ignores its context is still
// caught by the watchdog exactly as before. What changes is only that slow is
// no longer treated as dead.
//
// Here rather than beside the idle monitor because it is not the only loop
// that suspends: the autoscaler's scale-down is the same upload inside the
// same 30s budget, for the replicas the idle monitor steps aside for.
func VouchWhile(ctx context.Context, budget, every time.Duration, tick func(),
	unit func(context.Context) error) error {

	uctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- unit(uctx) }()

	beat := time.NewTicker(every)
	defer beat.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-beat.C:
			if uctx.Err() == nil {
				tick()
			}
		}
	}
}
