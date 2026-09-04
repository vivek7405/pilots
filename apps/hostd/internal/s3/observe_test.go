package s3

import (
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// The package has no client tests -- there is no object storage to talk to --
// so observe is asserted on its own. It is the only thing between an S3 call
// and the scrape, and a call that is timed but never counted, or counted under
// the wrong op, is invisible until someone reads a dashboard.
//
// Deltas, not absolutes: the registry is package-level and shared.
func TestObserveCountsAndTimesOneCall(t *testing.T) {
	beforeOps := metrics.S3Ops.With("get").Load()
	beforeCalls := metrics.S3OpSeconds.With("get").Count()

	observe("get", time.Now().Add(-time.Second))

	if got := metrics.S3Ops.With("get").Load() - beforeOps; got != 1 {
		t.Errorf("op count grew by %d, want 1", got)
	}
	if got := metrics.S3OpSeconds.With("get").Count() - beforeCalls; got != 1 {
		t.Errorf("latency observations grew by %d, want 1", got)
	}
	// A different op is a different series, not the same one.
	if n := metrics.S3Ops.With("put").Load(); n != 0 {
		t.Errorf("an unrelated op counted %d, want 0", n)
	}
}
