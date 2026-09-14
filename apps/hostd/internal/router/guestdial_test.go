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

	conn, err := dialGuest(context.Background(), &net.Dialer{}, "tcp", addr, 5*time.Second)
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
	_, err := dialGuest(context.Background(), &net.Dialer{}, "tcp", "127.0.0.1:1", 400*time.Millisecond)
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
	if _, err := dialGuest(ctx, &net.Dialer{}, "tcp", "127.0.0.1:1", time.Minute); err == nil {
		t.Fatal("a cancelled dial reported success")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("a cancelled dial kept retrying: %v", time.Since(start))
	}
}
