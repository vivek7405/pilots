package certs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// DomainSource is the fleet's set of custom hostnames, read locally.
type DomainSource interface {
	ListDomains(ctx context.Context) ([]state.Domain, error)
}

// Decider answers "may this host obtain a certificate for this name".
//
// It is called on EVERY TLS handshake for an unrecognised SNI, which makes it
// the whole abuse defence: wildcard DNS points *.pilotrun.app at every host,
// so anyone can aim a hostname at our IPs and open a connection. A permissive
// answer turns the fleet into a free certificate mint and burns the Let's
// Encrypt rate limit for every real customer on it.
//
// So it must be a LOCAL read -- no network, no round trip to another host, no
// call to the dashboard -- and it must refuse anything it does not recognise.
// The cache exists because a handshake is on the hot path and the underlying
// read is a Corrosion query; a second of staleness is irrelevant to a name
// that was just registered through the API.
type Decider struct {
	src DomainSource
	ttl time.Duration

	mu      sync.RWMutex
	known   map[string]struct{}
	fetched time.Time
}

func NewDecider(src DomainSource) *Decider {
	return &Decider{src: src, ttl: time.Second, known: map[string]struct{}{}}
}

// Allow is certmagic's DecisionFunc.
func (d *Decider) Allow(ctx context.Context, name string) error {
	name = strings.ToLower(strings.TrimSuffix(name, "."))

	if err := d.refresh(ctx); err != nil {
		// Refusing on a failed read is the safe direction: the cost of a
		// wrongly refused handshake is one retry, and the cost of a wrongly
		// allowed one is an unbounded issuance the whole fleet pays for.
		return fmt.Errorf("certs: cannot check %q against the fleet's domains: %w", name, err)
	}

	d.mu.RLock()
	_, ok := d.known[name]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("certs: %q is not a registered custom domain", name)
	}
	return nil
}

func (d *Decider) refresh(ctx context.Context) error {
	d.mu.RLock()
	fresh := time.Since(d.fetched) < d.ttl
	d.mu.RUnlock()
	if fresh {
		return nil
	}

	domains, err := d.src.ListDomains(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(domains))
	for _, dom := range domains {
		// Unverified names are excluded. A row exists as soon as someone asks
		// for a domain, but the CNAME check is what proves they control it --
		// issuing before that spends the fleet's rate limit on a name its
		// owner may never point here.
		if dom.VerifiedAt == 0 {
			continue
		}
		known[strings.ToLower(dom.Hostname)] = struct{}{}
	}

	d.mu.Lock()
	d.known, d.fetched = known, time.Now()
	d.mu.Unlock()
	return nil
}
