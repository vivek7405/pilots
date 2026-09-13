package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
)

// fakeNotifySocket stands in for systemd: a datagram socket hostd writes to,
// and a channel carrying what it wrote.
func fakeNotifySocket(t *testing.T) <-chan string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenPacket("unixgram", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", path)

	msgs := make(chan string, 16)
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				close(msgs)
				return
			}
			msgs <- string(buf[:n])
		}
	}()
	return msgs
}

func TestWatchdogIntervalIsAThirdOfTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		usec string
		want time.Duration
		ok   bool
	}{
		{"30000000", 10 * time.Second, true},
		{"3000000", time.Second, true},
		// Never below a second, whatever systemd says: petting in a tight loop
		// would cost more than the watchdog is worth.
		{"600000", time.Second, true},
		{"", 0, false},
		{"nonsense", 0, false},
		{"0", 0, false},
	} {
		got, ok := watchdogInterval(tc.usec)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("watchdogInterval(%q) = %v, %v; want %v, %v", tc.usec, got, ok, tc.want, tc.ok)
		}
	}
}

// A healthy daemon pets, which is the half that keeps it alive.
func TestTheWatchdogPetsWhileTheLoopsAreHealthy(t *testing.T) {
	msgs := fakeNotifySocket(t)
	t.Setenv("WATCHDOG_USEC", "3000000") // pets every second

	l := metrics.NewLoop("test_healthy_loop", time.Hour)
	l.Tick()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWatchdog(ctx)

	select {
	case got := <-msgs:
		if got != "WATCHDOG=1\n" {
			t.Errorf("sent %q, want WATCHDOG=1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a healthy daemon never petted the watchdog, so systemd would restart it")
	}
}

// The half that matters, and the one `Restart=always` cannot do: a loop that
// has stopped ticking withholds the pet, so systemd restarts a daemon that is
// up and doing nothing.
func TestTheWatchdogWithholdsThePetWhileALoopIsStalled(t *testing.T) {
	msgs := fakeNotifySocket(t)
	t.Setenv("WATCHDOG_USEC", "3000000")

	// A loop registered with a budget already behind it: never ticked, so it
	// is overdue from the first pass.
	metrics.NewLoop("test_stalled_loop", time.Nanosecond)
	t.Cleanup(func() { metrics.NewLoop("test_stalled_loop", time.Nanosecond).Tick() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWatchdog(ctx)

	select {
	case got, ok := <-msgs:
		if ok {
			t.Fatalf("the watchdog petted %q while a loop was stalled; systemd "+
				"would keep a wedged daemon alive", got)
		}
	case <-time.After(3 * time.Second):
		// Nothing sent across three pet intervals, which is the assertion.
	}
}

// A host started outside systemd has no watchdog and must not spin.
func TestTheWatchdogIsInertWithoutSystemd(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	done := make(chan struct{})
	go func() {
		runWatchdog(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("runWatchdog did not return on a host with no watchdog configured")
	}
}
