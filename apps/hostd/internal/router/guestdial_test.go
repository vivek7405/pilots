package router

import (
	"context"
	"net"
	"testing"
	"time"
)

// A guest that starts listening a moment after the first connect is reached,
// rather than answered with a 502. This is the shape of a just-woken machine:
// running, and not yet answering on the network.
func TestDialGuestWaitsForAGuestComingUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // refused until the guest "comes up" below

	up := make(chan net.Listener, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			up <- nil
			return
		}
		up <- l
	}()

	conn, err := dialGuest(context.Background(), (&net.Dialer{}).DialContext, "tcp", addr, 5*time.Second)
	if l := <-up; l != nil {
		defer l.Close()
	} else {
		t.Skip("the port was taken before the fake guest could bind it")
	}
	if err != nil {
		t.Fatalf("a guest that came up 300ms in was reported unreachable: %v", err)
	}
	conn.Close()
}

// And a guest that never comes up still fails, inside the window, so a dead
// machine is a 502 rather than a request held until the client gives up.
func TestDialGuestGivesUp(t *testing.T) {
	start := time.Now()
	_, err := dialGuest(context.Background(), (&net.Dialer{}).DialContext, "tcp", "127.0.0.1:1", 400*time.Millisecond)
	if err == nil {
		t.Fatal("a port nothing listens on was reported reachable")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the retry overran its window: %v", time.Since(start))
	}
}

// A cancelled request stops retrying at once.
func TestDialGuestHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := dialGuest(ctx, (&net.Dialer{}).DialContext, "tcp", "127.0.0.1:1", time.Minute); err == nil {
		t.Fatal("a cancelled dial reported success")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a cancelled dial kept retrying: %v", time.Since(start))
	}
}

// A SYN sent before a resumed guest processes packets is lost without an
// error, and a plain connect then waits out the kernel's one-second SYN
// retransmit. The dial must abandon an unanswered attempt and try again well
// before that.
func TestDialGuestRetriesAnUnansweredAttemptQuickly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	attempts := 0
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		attempts++
		if attempts == 1 {
			<-ctx.Done() // the lost SYN: nothing answers until the attempt gives up
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}

	start := time.Now()
	conn, err := dialGuest(context.Background(), dial, "tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("a guest that answered the second SYN was reported unreachable: %v", err)
	}
	conn.Close()
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("an unanswered attempt cost %v; want it abandoned in about 100ms", took)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}
