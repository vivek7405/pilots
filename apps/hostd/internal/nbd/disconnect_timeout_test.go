package nbd

import (
	"strings"
	"testing"
	"time"
)

// The parent-side teardown RETURNS, even when the device will not.
//
// Opening /dev/nbdN blocks in the kernel with no deadline of its own when
// another process is already inside nbd_disconnect on that device and holds
// the block device's mutex. Stop's comment warns that a missed disconnect
// leaves a handler in D-state and the device unusable until a reboot; what it
// did not anticipate is that the disconnect ITSELF can hang.
//
// Measured: after a battery run left one handler stuck in nbd_disconnect,
// hostd's next start blocked here on goroutine 1 -- the startup path -- for
// twenty-four minutes and never listened. One leaked device took the whole
// host down, and nothing said why.
//
// The index is one no host has: DisconnectDevice must come back with an error
// rather than sit on a missing device, and it must come back QUICKLY, which is
// the half that matters. Removing the select makes this hang or block on the
// open instead of returning.
func TestDisconnectDeviceAlwaysReturns(t *testing.T) {
	start := time.Now()
	err := DisconnectDevice(9999)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("disconnecting a device that does not exist reported success")
	}
	if elapsed > disconnectTimeout+2*time.Second {
		t.Errorf("took %v, which is past the %v bound; the startup path can block "+
			"on this and a host that cannot start is worse than a leaked device",
			elapsed, disconnectTimeout)
	}
}

// The bound is long enough that a healthy device is never cut short. Ten
// seconds is orders of magnitude more than three ioctls take, so the timeout
// can only ever fire on a device that is genuinely wedged.
func TestTheDisconnectBoundIsNotTight(t *testing.T) {
	if disconnectTimeout < 5*time.Second {
		t.Errorf("disconnectTimeout is %v; a healthy teardown must never race it",
			disconnectTimeout)
	}
}

// And when it does fire, it says what happened and what it costs, because the
// caller logs this and carries on -- so the log line is the only record that a
// device was leaked.
func TestTheTimeoutSaysWhatWasLeaked(t *testing.T) {
	err := DisconnectDevice(9999)
	if err == nil {
		t.Fatal("no error")
	}
	// Either message is acceptable here (the device is absent on a test box,
	// so the open fails fast); what must never happen is a bare or empty one.
	if !strings.Contains(err.Error(), "nbd") {
		t.Errorf("the error does not name the subsystem: %v", err)
	}
}
