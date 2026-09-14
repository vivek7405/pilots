package router

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// guestDialWindow bounds how long a dial to a guest keeps retrying a refusal or
// an unreachable address before the request becomes a 502.
//
// A guest that has just been restored is running before its network answers.
// Its namespace is rebuilt on wake with an empty neighbour table, so the first
// connect has to resolve the guest's address, and the resumed guest does not
// always answer the first round of ARP: the kernel gives up after three probes
// and connect returns EHOSTUNREACH three seconds in. Measured on a laptop, two
// wakes in five of a webjs replica answered its held request with a 502 for
// exactly that reason, although the wake itself took 270 ms and the next
// request was served. A held request must never see a waiting page, and a 502
// is worse than one.
//
// Retrying the DIAL is safe for every request, idempotent or not: nothing of
// the request has reached the guest when a connect fails. Bounded, so a guest
// that is really gone still fails rather than holding the client, and well
// inside HeldWakeWindow.
const guestDialWindow = 15 * time.Second

// guestTransport is the transport the proxy reaches guests through.
var guestTransport = newGuestTransport(guestDialWindow)

func newGuestTransport(window time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialGuest(ctx, dialer, network, addr, window)
	}
	return t
}

// dialGuest dials, retrying the two errors a guest that is still coming up
// produces. Anything else -- a timeout, a cancelled request -- is returned at
// once, because retrying it would only spend the client's time.
func dialGuest(ctx context.Context, d *net.Dialer, network, addr string,
	window time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(window)
	backoff := 25 * time.Millisecond
	for {
		conn, err := d.DialContext(ctx, network, addr)
		if err == nil || !guestNotReady(err) || time.Now().Add(backoff).After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}

// guestNotReady reports a connect error a guest still coming up produces: no
// neighbour entry yet, or its agent not listening yet.
func guestNotReady(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ECONNREFUSED)
}
