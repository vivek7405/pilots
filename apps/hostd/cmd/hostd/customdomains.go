package main

import (
	"context"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// customDomainTTL is how stale the hostname index may be.
//
// A domain becomes routable this long after it verifies, at worst, which is
// nothing next to the DNS propagation that preceded it. It is also the most
// often an UNKNOWN Host header can make this host read its store: the index
// refreshes on a clock, never on a miss, so a scanner sending random
// hostnames costs a map lookup each.
const customDomainTTL = 10 * time.Second

// customDomainRetry is how soon a FAILED refresh is tried again.
const customDomainRetry = time.Second

// customDomains maps a verified custom hostname to the address label of the
// service it belongs to, from the local replica only (rule 2): the request
// path must not depend on any other host, and this is on it twice, once in
// dispatch and once in the router's resolve.
//
// Verified rows only. An unverified hostname is one somebody claimed without
// proving they control it; routing it would serve one tenant's service on a
// name another tenant may own.
type customDomains struct {
	store state.Store
	now   func() time.Time

	mu      sync.Mutex
	at      time.Time
	byHost  map[string]string
	loading bool
}

func newCustomDomains(store state.Store) *customDomains {
	return &customDomains{store: store, now: time.Now}
}

// Label is the service label a hostname stands for. The key is the form
// router.NormalizeHost produces.
func (c *customDomains) Label(host string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byHost == nil {
		// The first read blocks, and blocks EVERY caller, because there is
		// nothing to answer from: it runs under the lock, so a request that
		// arrives while it is in flight waits for the index instead of reading
		// a nil map as "not one of ours" and being answered 401 by the control
		// API. That is every custom domain's first seconds after each restart.
		c.apply(c.load())
	} else if !c.loading && c.now().Sub(c.at) >= customDomainTTL {
		// Every later one refreshes behind the request that noticed, which is
		// served from the index it found: a request never waits on the store
		// for a hostname the index already knows.
		c.loading = true
		go func() {
			next, err := c.load()
			c.mu.Lock()
			defer c.mu.Unlock()
			c.loading = false
			c.apply(next, err)
		}()
	}
	label, ok := c.byHost[host]
	return label, ok
}

// apply installs a loaded index, or keeps the last good one. Called with the
// lock held.
func (c *customDomains) apply(next map[string]string, err error) {
	if err != nil {
		// Keep what we had. A store that is briefly unwell must not turn a
		// live custom domain into a 401 from the control API.
		//
		// And look again in a second rather than in a whole TTL. Still on a
		// clock and never on a miss, but a store that was not up yet when this
		// host started must not cost every custom domain its first ten seconds.
		c.at = c.now().Add(customDomainRetry - customDomainTTL)
		if c.byHost == nil {
			c.byHost = map[string]string{}
		}
		return
	}
	c.byHost, c.at = next, c.now()
}

func (c *customDomains) load() (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	domains, err := c.store.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(domains))
	if len(domains) == 0 {
		return out, nil
	}
	services, err := c.store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(services))
	for _, s := range services {
		labels[s.ID] = s.Domain
	}
	for _, d := range domains {
		if d.VerifiedAt == 0 {
			continue
		}
		if label := labels[d.ServiceID]; label != "" {
			out[d.Hostname] = label
		}
	}
	return out, nil
}
