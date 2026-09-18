package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// domainStore answers the two reads the index makes and counts them.
type domainStore struct {
	state.Store
	domains  []state.Domain
	services []state.Service
	fail     atomic.Bool
	reads    atomic.Int64
	// gate, when set, holds every ListDomains until it is closed.
	gate chan struct{}
}

func (s *domainStore) ListDomains(context.Context) ([]state.Domain, error) {
	s.reads.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	if s.fail.Load() {
		return nil, errors.New("store unwell")
	}
	return s.domains, nil
}

func (s *domainStore) ListServices(context.Context) ([]state.Service, error) {
	return s.services, nil
}

func newIndex(st *domainStore) (*customDomains, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	c := newCustomDomains(st)
	c.now = func() time.Time { return now }
	return c, &now
}

// Only a VERIFIED hostname routes. An unverified row is a claim nobody has
// proved, and routing it would serve one tenant's service on a name another
// may own.
func TestOnlyVerifiedHostnamesAreIndexed(t *testing.T) {
	st := &domainStore{
		services: []state.Service{{ID: "s-1", Domain: "web"}},
		domains: []state.Domain{
			{Hostname: "pilots.run", ServiceID: "s-1", VerifiedAt: 1},
			{Hostname: "claimed.example.com", ServiceID: "s-1"},
			{Hostname: "orphan.example.com", ServiceID: "s-gone", VerifiedAt: 1},
		},
	}
	c, _ := newIndex(st)
	if label, ok := c.Label("pilots.run"); !ok || label != "web" {
		t.Fatalf("pilots.run -> %q %v, want web", label, ok)
	}
	if _, ok := c.Label("claimed.example.com"); ok {
		t.Error("an unverified hostname routes")
	}
	if _, ok := c.Label("orphan.example.com"); ok {
		t.Error("a hostname whose service is gone routes")
	}
}

// Unknown Host headers must not buy a store read each: the index refreshes on
// a clock, never on a miss, or a scanner turns random hostnames into load.
func TestAMissDoesNotReadTheStore(t *testing.T) {
	st := &domainStore{}
	c, _ := newIndex(st)
	for i := 0; i < 500; i++ {
		c.Label("random-host.example.com")
	}
	if n := st.reads.Load(); n != 1 {
		t.Fatalf("500 misses inside one TTL read the store %d times, want once", n)
	}
}

// A store that is briefly unwell keeps the last good answer: a live custom
// domain must not fall to the control API's 401 because one read failed.
func TestAFailedRefreshKeepsTheLastIndex(t *testing.T) {
	st := &domainStore{
		services: []state.Service{{ID: "s-1", Domain: "web"}},
		domains:  []state.Domain{{Hostname: "pilots.run", ServiceID: "s-1", VerifiedAt: 1}},
	}
	c, now := newIndex(st)
	if _, ok := c.Label("pilots.run"); !ok {
		t.Fatal("not indexed")
	}
	st.fail.Store(true)
	*now = now.Add(2 * customDomainTTL)
	c.Label("pilots.run") // notices it is stale and refreshes behind the request
	deadline := time.Now().Add(2 * time.Second)
	for st.reads.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := c.Label("pilots.run"); !ok {
		t.Fatal("a failed refresh dropped a verified hostname")
	}
}

// A hostname verified after the index was built becomes routable within a TTL.
func TestANewlyVerifiedHostnameAppearsAfterTheTTL(t *testing.T) {
	st := &domainStore{services: []state.Service{{ID: "s-1", Domain: "web"}}}
	c, now := newIndex(st)
	if _, ok := c.Label("pilots.run"); ok {
		t.Fatal("routed before it existed")
	}
	st.domains = []state.Domain{{Hostname: "pilots.run", ServiceID: "s-1", VerifiedAt: 1}}
	*now = now.Add(2 * customDomainTTL)
	c.Label("pilots.run")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.Label("pilots.run"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a verified hostname never became routable")
}

// A request that arrives while the FIRST load is still in flight waits for it.
//
// It used to read the nil index as a miss, so after every restart the requests
// racing the first one were sent to the control API and answered 401 on a
// hostname that was verified and routable.
func TestARequestDuringTheFirstLoadWaitsForIt(t *testing.T) {
	st := &domainStore{
		services: []state.Service{{ID: "s-1", Domain: "web"}},
		domains:  []state.Domain{{Hostname: "pilots.run", ServiceID: "s-1", VerifiedAt: 1}},
		gate:     make(chan struct{}),
	}
	c, _ := newIndex(st)

	answers := make(chan bool, 2)
	go func() { _, ok := c.Label("pilots.run"); answers <- ok }()
	for st.reads.Load() == 0 { // the first load is now inside the store
		time.Sleep(time.Millisecond)
	}
	go func() { _, ok := c.Label("pilots.run"); answers <- ok }()
	time.Sleep(20 * time.Millisecond) // let the second one reach the index
	close(st.gate)

	for i := 0; i < 2; i++ {
		if ok := <-answers; !ok {
			t.Fatal("a request racing the first load was told the hostname is not ours")
		}
	}
}

// A failed refresh is tried again in a second, not in a whole TTL: a store
// that was not up when the host started must not cost every custom domain its
// first ten seconds.
func TestAFailedLoadIsRetriedSoonerThanTheTTL(t *testing.T) {
	st := &domainStore{
		services: []state.Service{{ID: "s-1", Domain: "web"}},
		domains:  []state.Domain{{Hostname: "pilots.run", ServiceID: "s-1", VerifiedAt: 1}},
	}
	st.fail.Store(true)
	c, now := newIndex(st)
	if _, ok := c.Label("pilots.run"); ok {
		t.Fatal("routed from a store that could not be read")
	}
	st.fail.Store(false)
	*now = now.Add(customDomainRetry)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.Label("pilots.run"); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a failed first load was not retried after customDomainRetry")
}
