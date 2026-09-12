package fc

import (
	"testing"
	"time"
)

// A waiter blocks until the capture finishes.
//
// # The bug this exists for
//
// A checkpoint's expensive half runs in the background, reading staged copies
// out of the machine's cache directory. Destroy and Redeploy remove that whole
// tree, and neither waited for it. The goroutine went on reading a directory
// that had been deleted underneath it and failed part-way through:
//
//	block: open .../checkpoints/<id>/rootfs.cow: no such file or directory
//
// The rig's journal carries thirteen of these over a week, in three flavours
// -- rootfs.cow, snap.bin, builds.json -- which is the same race caught at
// three points in the upload. Killing the VMM does not stop it, because it
// reads files rather than the guest.
func TestAwaitCaptureWaitsForTheCaptureToFinish(t *testing.T) {
	m := &Machine{ID: "m-test"}
	m.beginCapture()

	finished := make(chan struct{})
	go func() {
		// Long enough that a caller which did not wait would win the race
		// and remove the files this stands in for.
		time.Sleep(60 * time.Millisecond)
		close(finished)
		m.endCapture()
	}()

	if !m.AwaitCapture(5 * time.Second) {
		t.Fatal("AwaitCapture reported the capture unfinished inside a 5s bound")
	}
	select {
	case <-finished:
	default:
		t.Fatal("AwaitCapture returned before the capture finished, so a destroy " +
			"would remove the tree the upload is still reading")
	}
}

// The wait is BOUNDED, or a wedged upload holds a destroy open forever.
//
// A caller who asked for a machine to go away would rather it went away with
// an upload half finished than not at all.
func TestAwaitCaptureGivesUpAtItsBound(t *testing.T) {
	m := &Machine{ID: "m-test"}
	m.beginCapture()
	t.Cleanup(m.endCapture)

	start := time.Now()
	if m.AwaitCapture(40 * time.Millisecond) {
		t.Fatal("AwaitCapture claimed a capture finished that never did")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("AwaitCapture waited %v past a 40ms bound", waited)
	}
}

// Nothing in flight is not something to wait for, which is the ordinary case:
// most destroys follow no checkpoint at all.
func TestAwaitCaptureReturnsAtOnceWithNothingInFlight(t *testing.T) {
	m := &Machine{ID: "m-test"}

	start := time.Now()
	if !m.AwaitCapture(5 * time.Second) {
		t.Fatal("AwaitCapture reported an absent capture as unfinished")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("AwaitCapture took %v with no capture in flight; every destroy "+
			"on this host would pay that", waited)
	}
}

// An unbounded wait is what the snapshot path uses, where waiting longer is
// simply correct: it is waiting for work it is about to compete with.
func TestABoundOfZeroWaitsAsLongAsItTakes(t *testing.T) {
	m := &Machine{ID: "m-test"}
	m.beginCapture()

	go func() {
		time.Sleep(50 * time.Millisecond)
		m.endCapture()
	}()

	done := make(chan bool, 1)
	go func() { done <- m.AwaitCapture(0) }()

	select {
	case ok := <-done:
		if !ok {
			t.Error("an unbounded wait reported the capture unfinished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unbounded wait did not return after the capture ended")
	}
}
