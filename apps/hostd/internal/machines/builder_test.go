package machines

import (
	"context"
	"net"
	"testing"
	"time"
)

// A running machine is not the same as a daemon accepting connections. Without
// this wait buildctl fails instantly with a refused connection, which reads
// like a networking fault rather than a daemon that has not finished starting.
func TestWaitForBuildkitReturnsOnceSomethingListens(t *testing.T) {
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

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	if err := waitForBuildkitAddr(context.Background(), net.JoinHostPort(host, port), 5*time.Second); err != nil {
		t.Fatalf("a listening port was reported unreachable: %v", err)
	}
}

// And it gives up rather than hanging, so a builder that will never answer
// fails the build inside the build's own timeout.
func TestWaitForBuildkitGivesUp(t *testing.T) {
	// Port 1 on loopback: nothing listens, and a connect fails immediately
	// rather than hanging, so this costs the timeout and no more.
	start := time.Now()
	err := waitForBuildkitAddr(context.Background(), "127.0.0.1:1", 300*time.Millisecond)
	if err == nil {
		t.Fatal("a dead address was reported reachable")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the wait overran its timeout by too much: %v", time.Since(start))
	}
}

// A cancelled context stops the wait at once: a client that hung up should not
// leave hostd polling a guest for a minute and a half.
func TestWaitForBuildkitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForBuildkitAddr(ctx, "127.0.0.1:1", time.Minute); err == nil {
		t.Fatal("a cancelled wait reported success")
	}
}
