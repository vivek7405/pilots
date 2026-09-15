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
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialGuest(ctx, dialer.DialContext, network, addr, window)
	}
	return t
}

// Per-attempt connect timeouts, shortest first.
//
// A guest is on this host, one veth away: a live one completes a handshake in
// well under a millisecond. The danger is the one that is not live YET. A
// resumed guest takes a few hundred milliseconds to start processing packets,
// and a SYN sent into that window is simply lost -- no refusal, no error -- so
// a plain connect sits in the kernel's initial SYN retransmit timeout, which
// is one second. Traced on a laptop wake: the app answered a direct probe at
// +687 ms, and the router's request, whose SYN had gone out at +140 ms, landed
// at +1217 ms. Abandoning an unanswered attempt after 100 ms and sending a
// fresh SYN takes that second back. Growing, so a guest that is merely slow to
// accept is not hammered, and capped so the whole window still applies.
var guestAttemptTimeouts = []time.Duration{
	100 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	400 * time.Millisecond, time.Second,
}

// dialGuest dials, retrying what a guest that is still coming up produces: a
// refusal, an unreachable address, or an attempt nobody answered. A cancelled
// request is returned at once, because retrying it would only spend the
// client's time.
func dialGuest(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error),
	network, addr string, window time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(window)
	backoff := 25 * time.Millisecond
	for attempt := 0; ; attempt++ {
		timeout := guestAttemptTimeouts[min(attempt, len(guestAttemptTimeouts)-1)]
		if left := time.Until(deadline); left < timeout {
			timeout = max(left, time.Millisecond)
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(actx, network, addr)
		// This attempt's own timeout, read BEFORE cancel: afterwards Err is
		// always Canceled, every failure would look unanswered, a refusal
		// would be retried in a tight loop with no backoff, and a permanent
		// error would be retried for the whole window before the 502.
		unanswered := errors.Is(actx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if (!guestNotReady(err) && !unanswered) || time.Now().Add(backoff).After(deadline) {
			return conn, err
		}
		if unanswered {
			continue // the timeout already waited; resend the SYN now
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
