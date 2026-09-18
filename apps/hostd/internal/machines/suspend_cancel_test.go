package machines

import (
	"os"
	"strings"
	"testing"
)

// Once the Firecracker is dead, a suspend can no longer be cancelled.
//
// The idle monitor and the autoscaler run a suspend under a deadline. If that
// deadline passes while the memory image is still uploading, SuspendInstant
// can return success with the context already expired: the process is gone and
// the image is in the bucket. Every write after that point has to land, or the
// row goes on saying running for a machine with no process, the next pass
// marks it stopped, and it cold-boots instead of waking from an image that was
// uploaded successfully.
//
// No behavioural test can reach this here, for the reason callsites_test.go
// gives: SuspendInstant needs a Firecracker host. So this holds the ORDER in
// the source: the context is detached after the capture returns and before the
// machine is dropped and its row written. Move or delete the WithoutCancel line
// and this fails.
func TestASuspendCannotBeCancelledAfterTheKill(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (m *Manager) suspendLocked(")
	if start < 0 {
		t.Fatal("suspendLocked is gone; this test guards its body")
	}
	body := text[start:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}

	at := func(needle string) int {
		i := strings.Index(body, needle)
		if i < 0 {
			t.Fatalf("suspendLocked no longer contains %q", needle)
		}
		return i
	}
	capture := at("fcm.SuspendInstant(")
	detach := at("ctx = context.WithoutCancel(ctx)")
	drop := at("m.drop(id)")
	// The LAST write: an earlier one corrects a row whose process is already
	// gone, and returns before any capture.
	write := strings.LastIndex(body, "m.opts.Store.PutMachine(ctx, row)")
	if write < 0 {
		t.Fatal("suspendLocked no longer writes the machine row")
	}

	if !(capture < detach && detach < drop && drop < write) {
		t.Fatalf("order is capture=%d detach=%d drop=%d write=%d: the context must be "+
			"detached after the capture and before the machine is dropped and its row "+
			"written, or a deadline that passes mid-upload strands a dead machine as running",
			capture, detach, drop, write)
	}
}
