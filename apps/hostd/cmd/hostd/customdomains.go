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
	stale := c.byHost == nil || c.now().Sub(c.at) >= customDomainTTL
	if stale && !c.loading {
		c.loading = true
		first := c.byHost == nil
		c.mu.Unlock()
		// The first read blocks, because there is nothing to answer from. Every
		// later one refreshes behind the request that noticed, which is served
		// from the index it found: a request never waits on the store for a
		// hostname the index already knows.
		if first {
			c.refresh()
		} else {
			go c.refresh()
		}
		c.mu.Lock()
	}
	label, ok := c.byHost[host]
	c.mu.Unlock()
	return label, ok
}

func (c *customDomains) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	next, err := c.load(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.loading = false
	c.at = c.now()
	if err != nil {
		// Keep what we had. A store that is briefly unwell must not turn a
		// live custom domain into a 401 from the control API.
		if c.byHost == nil {
			c.byHost = map[string]string{}
		}
		return
	}
	c.byHost = next
}

func (c *customDomains) load(ctx context.Context) (map[string]string, error) {
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
