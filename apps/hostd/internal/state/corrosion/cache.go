package corrosion

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The router, the idle monitor and the self-heal loop all need the cluster's
// current view, and the router needs it on every request. Querying the agent
// each time would be a syscall and a round trip on the hot path; opening
// corrosion's SQLite file read-only would mean a second reader against a
// database whose cr-sqlite triggers and shadow tables the agent owns.
//
// So: one subscription per table, materialized into a map at startup and kept
// current by the change stream. Reads are a mutex and a map lookup.

// rebuildDelay is how long to wait before rebuilding a cache whose
// subscription died. Long enough not to hammer a restarting agent.
const rebuildDelay = time.Second

// Cache is a live in-memory view of the cluster.
type Cache struct {
	client *Client

	mu       sync.RWMutex
	machines map[string]state.Machine
	hosts    map[string]state.Host
	// services carries ONLY id, name and app -- what .internal needs to turn a
	// service name into its replicas. Deliberately not the whole row: a
	// service holds the sealed environment, and a second in-memory copy of
	// every app's secrets on every host is a cost with no reader.
	services map[string]state.Service
	// tenancy answers "which org owns this id" on every authenticated read.
	// A subscription rather than a query: org scoping runs on the API's hot
	// path, and a request path that queries the agent is a request path that
	// can block on it.
	tenancy map[string]state.Tenancy
	// revoked is the set of killed key hashes, checked on every request. Held
	// as a set because nothing reads the revocation time on this path.
	revoked map[string]struct{}
	// heardAt is when THIS host last saw a peer's heartbeat change, by the
	// local clock. Liveness is judged from it rather than from the last_seen
	// the peer wrote, because that value is stamped by the peer's clock and
	// compared against ours.
	//
	// Nothing keeps those two clocks together. A box whose time is a minute
	// behind heartbeats every five seconds and still reads as long dead to
	// every peer: they claim its machines while it is serving them, its own
	// release loop stops them, and ownership oscillates. Skewed the other way,
	// a genuinely dead host never looks dead and is never rescued from.
	//
	// An arrival is observed locally, so one clock decides -- and the same
	// clock that measures the interval.
	heardAt map[string]time.Time
	ready   bool
	now     func() time.Time

	// hostsChanged wakes anything that has to act on a host appearing or
	// leaving, rather than discover it on its next sweep. Buffered by one and
	// written without blocking: it says "something changed", not what, so a
	// pending signal already covers a second change.
	hostsChanged chan struct{}
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// noteHeartbeat records that a host's heartbeat moved, or that it is new.
//
// Called with the lock held. A row that arrives unchanged -- a gossip
// re-delivery, or an update to some other column -- is deliberately NOT
// treated as a sign of life: only the heartbeat advancing means the host is
// still there.
func (c *Cache) noteHeartbeat(h state.Host) {
	prev, known := c.hosts[h.ID]
	if !known || prev.LastSeen != h.LastSeen {
		c.heardAt[h.ID] = c.clock()
	}
}

// NewCache subscribes to machines and hosts and returns once both have been
// materialized.
//
// It returns only after the initial rows are in, so a caller that gets a Cache
// gets a populated one -- a router serving from an empty cache would 404 every
// machine on the host for as long as it took the first rows to arrive.
func NewCache(ctx context.Context, client *Client) (*Cache, error) {
	c := &Cache{
		client:       client,
		machines:     map[string]state.Machine{},
		hosts:        map[string]state.Host{},
		services:     map[string]state.Service{},
		tenancy:      map[string]state.Tenancy{},
		revoked:      map[string]struct{}{},
		heardAt:      map[string]time.Time{},
		hostsChanged: make(chan struct{}, 1),
	}

	machines, err := c.subscribeMachines(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := c.subscribeHosts(ctx)
	if err != nil {
		machines.Close()
		return nil, err
	}
	services, err := c.subscribeServices(ctx)
	if err != nil {
		machines.Close()
		hosts.Close()
		return nil, err
	}
	tenancy, err := c.subscribeTenancy(ctx)
	if err != nil {
		machines.Close()
		hosts.Close()
		services.Close()
		return nil, err
	}
	revocations, err := c.subscribeRevocations(ctx)
	if err != nil {
		machines.Close()
		hosts.Close()
		services.Close()
		tenancy.Close()
		return nil, err
	}

	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()

	go c.follow(ctx, machines, "machines", c.subscribeMachines)
	go c.follow(ctx, hosts, "hosts", c.subscribeHosts)
	go c.follow(ctx, services, "services", c.subscribeServices)
	go c.follow(ctx, tenancy, "tenancy", c.subscribeTenancy)
	go c.follow(ctx, revocations, "api_key_revocations", c.subscribeRevocations)
	return c, nil
}

// subscribeTenancy materializes the owner of every machine, service and volume.
//
// Three columns, not four: created_at has no reader on the request path.
func (c *Cache) subscribeTenancy(ctx context.Context) (*Subscription, error) {
	sub, err := c.client.Subscribe(ctx, `SELECT id, org_id, kind FROM tenancy`)
	if err != nil {
		return nil, err
	}

	fresh := map[string]state.Tenancy{}
	rows := sub.Rows()
	for rows.Next() {
		var t state.Tenancy
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Kind); err != nil {
			sub.Close()
			return nil, err
		}
		fresh[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		sub.Close()
		return nil, err
	}

	c.mu.Lock()
	c.tenancy = fresh
	c.mu.Unlock()
	return sub, nil
}

// subscribeRevocations materializes the killed keys.
//
// A rebuild after a lost subscription re-reads the whole table, so a
// revocation cannot be missed by the gap: the tombstone is a row that only
// ever appears, and a full re-read always finds it.
func (c *Cache) subscribeRevocations(ctx context.Context) (*Subscription, error) {
	sub, err := c.client.Subscribe(ctx, `SELECT hash FROM api_key_revocations`)
	if err != nil {
		return nil, err
	}

	fresh := map[string]struct{}{}
	rows := sub.Rows()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			sub.Close()
			return nil, err
		}
		fresh[hash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		sub.Close()
		return nil, err
	}

	c.mu.Lock()
	c.revoked = fresh
	c.mu.Unlock()
	return sub, nil
}

// subscribeMachines opens a subscription and replaces the machine map with its
// initial rows.
func (c *Cache) subscribeMachines(ctx context.Context) (*Subscription, error) {
	sub, err := c.client.Subscribe(ctx, `SELECT `+machineCols+` FROM machines`)
	if err != nil {
		return nil, err
	}

	// The change events sit behind these rows in the same stream, so they must
	// be drained in full before the subscription will hand over changes.
	fresh := map[string]state.Machine{}
	rows := sub.Rows()
	for rows.Next() {
		var m state.Machine
		if err := scanMachine(rows, &m); err != nil {
			sub.Close()
			return nil, err
		}
		fresh[m.ID] = m
	}
	if err := rows.Err(); err != nil {
		sub.Close()
		return nil, err
	}

	c.mu.Lock()
	c.machines = fresh
	c.mu.Unlock()
	return sub, nil
}

func (c *Cache) subscribeHosts(ctx context.Context) (*Subscription, error) {
	sub, err := c.client.Subscribe(ctx,
		`SELECT id, wg_addr, wg_pubkey, public_ip, cpu_free, mem_free_mib, last_seen FROM hosts`)
	if err != nil {
		return nil, err
	}

	fresh := map[string]state.Host{}
	rows := sub.Rows()
	for rows.Next() {
		var h state.Host
		if err := rows.Scan(&h.ID, &h.WGAddr, &h.WGPubKey, &h.PublicIP,
			&h.CPUFree, &h.MemFreeMiB, &h.LastSeen); err != nil {
			sub.Close()
			return nil, err
		}
		fresh[h.ID] = h
	}
	if err := rows.Err(); err != nil {
		sub.Close()
		return nil, err
	}

	c.mu.Lock()
	// Everything in the first snapshot counts as heard now. Their last_seen
	// values were written before this host was watching, so there is no
	// arrival to date them from -- and assuming the worst would have a
	// starting host rescue the whole fleet out from under itself. They get one
	// deadAfter window to prove they are still there.
	seen := c.clock()
	for id := range fresh {
		c.heardAt[id] = seen
	}
	for id := range c.heardAt {
		if _, ok := fresh[id]; !ok {
			delete(c.heardAt, id)
		}
	}
	c.hosts = fresh
	c.mu.Unlock()
	return sub, nil
}

// subscribeServices materializes the name half of .internal.
//
// Three columns, not the whole row. See the services field on Cache.
func (c *Cache) subscribeServices(ctx context.Context) (*Subscription, error) {
	sub, err := c.client.Subscribe(ctx, `SELECT id, name, app FROM services`)
	if err != nil {
		return nil, err
	}

	fresh := map[string]state.Service{}
	rows := sub.Rows()
	for rows.Next() {
		var svc state.Service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.App); err != nil {
			sub.Close()
			return nil, err
		}
		fresh[svc.ID] = svc
	}
	if err := rows.Err(); err != nil {
		sub.Close()
		return nil, err
	}

	c.mu.Lock()
	c.services = fresh
	c.mu.Unlock()
	return sub, nil
}

// follow applies changes to the cache, rebuilding when the stream is lost.
//
// A subscription the agent has forgotten cannot be resumed, and the changes
// missed while it was gone are not recoverable -- so the only correct response
// is a fresh subscription and a full rebuild from its initial rows. Continuing
// with a cache that has silently stopped receiving changes is the worse
// outcome: the router keeps proxying to a host that no longer owns the machine.
func (c *Cache) follow(ctx context.Context, sub *Subscription, table string,
	resubscribe func(context.Context) (*Subscription, error)) {

	for {
		changes, err := sub.Changes()
		if err != nil {
			slog.Error("cluster cache could not follow changes", "table", table, "err", err)
		} else {
			for change := range changes {
				c.apply(table, change)
			}
			err = sub.Err()
		}
		sub.Close()

		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ErrSubscriptionGone) {
			slog.Error("cluster cache subscription failed", "table", table, "err", err)
		}

		// Rebuild. Not a retry of the old subscription -- that id is gone --
		// and not an incremental catch-up, because there is no cursor to catch
		// up from.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(rebuildDelay):
			}
			next, rerr := resubscribe(ctx)
			if rerr == nil {
				slog.Info("cluster cache rebuilt from a fresh subscription", "table", table)
				sub = next
				break
			}
			slog.Error("cluster cache could not rebuild", "table", table, "err", rerr)
		}
	}
}

// apply folds one change into the cache.
func (c *Cache) apply(table string, change Change) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if table == "hosts" {
		// Non-blocking: a signal already waiting says the same thing.
		defer func() {
			select {
			case c.hostsChanged <- struct{}{}:
			default:
			}
		}()
	}

	switch table {
	case "machines":
		var m state.Machine
		if err := change.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs,
			&m.ImageRef, &m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain,
			&m.AppPort, &m.AgentPort, &m.AgentTokenHash, &m.MemBuildID,
			&m.RootfsBuildID, &m.TemplateMemBuildID, &m.TemplateRootfsBuildID,
			&m.VolumeID, &m.ServiceID, &m.ReleaseID, &m.App, &m.Slot,
			&m.LastActivity, &m.UpdatedAt); err != nil {
			slog.Error("cluster cache could not read a machine change", "err", err)
			return
		}
		if change.Kind == ChangeDelete {
			delete(c.machines, m.ID)
			return
		}
		c.machines[m.ID] = m

	case "hosts":
		var h state.Host
		if err := change.Scan(&h.ID, &h.WGAddr, &h.WGPubKey, &h.PublicIP,
			&h.CPUFree, &h.MemFreeMiB, &h.LastSeen); err != nil {
			slog.Error("cluster cache could not read a host change", "err", err)
			return
		}
		if change.Kind == ChangeDelete {
			delete(c.hosts, h.ID)
			delete(c.heardAt, h.ID)
			return
		}
		c.noteHeartbeat(h)
		c.hosts[h.ID] = h

	case "services":
		var svc state.Service
		if err := change.Scan(&svc.ID, &svc.Name, &svc.App); err != nil {
			slog.Error("cluster cache could not read a service change", "err", err)
			return
		}
		if change.Kind == ChangeDelete {
			delete(c.services, svc.ID)
			return
		}
		c.services[svc.ID] = svc

	case "tenancy":
		var t state.Tenancy
		if err := change.Scan(&t.ID, &t.OrgID, &t.Kind); err != nil {
			slog.Error("cluster cache could not read a tenancy change", "err", err)
			return
		}
		if change.Kind == ChangeDelete {
			delete(c.tenancy, t.ID)
			return
		}
		c.tenancy[t.ID] = t

	case "api_key_revocations":
		var hash string
		if err := change.Scan(&hash); err != nil {
			slog.Error("cluster cache could not read a revocation change", "err", err)
			return
		}
		// No delete case on purpose. A revocation is a tombstone: it only
		// ever appears, and un-revoking is minting a new key.
		c.revoked[hash] = struct{}{}
	}
}

// OrgOf returns the org that owns an id, and whether any row says so.
//
// false means the object predates tenancy, not that it is public: callers
// treat it as visible to admin alone.
func (c *Cache) OrgOf(id string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	t, ok := c.tenancy[id]
	if !ok {
		return "", false
	}
	return t.OrgID, true
}

// Revoked reports whether a key hash has been killed. Read on every
// authenticated request, from memory, with no query.
func (c *Cache) Revoked(hash string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.revoked[hash]
	return ok
}

// Services returns the id, name and app of every service in the fleet.
//
// Unordered on purpose. The one caller is the resolver, which filters by
// name and shuffles what it answers, so a sort here would run on every
// .internal query and have no reader.
func (c *Cache) Services() []state.Service {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]state.Service, 0, len(c.services))
	for _, svc := range c.services {
		out = append(out, svc)
	}
	return out
}

// Machine returns a machine by id. Destroyed machines are invisible.
func (c *Cache) Machine(id string) (state.Machine, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	m, ok := c.machines[id]
	if !ok || m.State == state.StateDestroyed {
		return state.Machine{}, false
	}
	return m, true
}

// MachineByName resolves the routing key a URL carries.
//
// Corrosion cannot enforce uniqueness, so two hosts that disagree about who
// owns a name during a membership change can each create a row with that name
// and a different id. The router must still route -- deterministically, and
// identically on every host, or the same URL splits across two machines
// depending on which host the client reached. Lowest id wins, and it is logged,
// because it means a name was allocated twice.
func (c *Cache) MachineByName(name string) (state.Machine, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var (
		found   state.Machine
		matches int
	)
	for _, m := range c.machines {
		if m.Name != name || m.State == state.StateDestroyed {
			continue
		}
		matches++
		if matches == 1 || m.ID < found.ID {
			found = m
		}
	}
	if matches == 0 {
		return state.Machine{}, false
	}
	if matches > 1 {
		slog.Error("two machines share a name; routing to the lowest id. A name "+
			"was allocated twice, which means two hosts disagreed about who owned it",
			"name", name, "matches", matches, "routing_to", found.ID)
	}
	return found, true
}

// Machines returns every live machine, ordered by id.
func (c *Cache) Machines() []state.Machine {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]state.Machine, 0, len(c.machines))
	for _, m := range c.machines {
		if m.State != state.StateDestroyed {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Hosts returns every known host, ordered by id.
func (c *Cache) Hosts() []state.Host {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]state.Host, 0, len(c.hosts))
	for _, h := range c.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Host returns one host.
func (c *Cache) Host(id string) (state.Host, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.hosts[id]
	return h, ok
}

// LiveHosts returns the hosts heartbeating recently enough to count, SORTED BY
// ID.
//
// The ordering is load-bearing, not cosmetic. Every survivor computes its own
// rank as its index in this list and rescues the slice of machines that hashes
// to it. Two hosts that order the list differently compute different ranks, and
// then either rescue the same machine or rescue none of it.
func (c *Cache) LiveHosts(now time.Time, deadAfter time.Duration) []state.Host {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]state.Host, 0, len(c.hosts))
	for _, h := range c.hosts {
		if heard, ok := c.heardAt[h.ID]; ok && now.Sub(heard) < deadAfter {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IsLive reports whether a host is heartbeating.
//
// Measured from when its heartbeat last ARRIVED here, for the reason on
// heardAt: last_seen is the peer's clock, and comparing it to ours makes a
// host's liveness a function of how well two machines agree about the time.
func (c *Cache) IsLive(id string, now time.Time, deadAfter time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if _, ok := c.hosts[id]; !ok {
		return false
	}
	heard, ok := c.heardAt[id]
	return ok && now.Sub(heard) < deadAfter
}

// HostsChanged fires when the hosts table changes.
//
// The mesh reconciles on a timer as a safety net, but a host that has just
// joined must become routable when its row ARRIVES, not up to one tick later:
// until then it can see the whole fleet through Corrosion while having no
// route to any of it, and every request it is asked to forward times out.
func (c *Cache) HostsChanged() <-chan struct{} { return c.hostsChanged }
