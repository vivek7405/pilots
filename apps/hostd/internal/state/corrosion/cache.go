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
		client:   client,
		machines: map[string]state.Machine{},
		hosts:    map[string]state.Host{},
		heardAt:  map[string]time.Time{},
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

	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()

	go c.follow(ctx, machines, "machines", c.subscribeMachines)
	go c.follow(ctx, hosts, "hosts", c.subscribeHosts)
	return c, nil
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

	switch table {
	case "machines":
		var m state.Machine
		if err := change.Scan(&m.ID, &m.Name, &m.HostID, &m.State, &m.KindKnobs,
			&m.ImageRef, &m.VCPUs, &m.MemMiB, &m.Domain, &m.CustomDomain,
			&m.AppPort, &m.AgentPort, &m.AgentTokenHash, &m.MemBuildID,
			&m.RootfsBuildID, &m.TemplateMemBuildID, &m.TemplateRootfsBuildID,
			&m.VolumeID, &m.ServiceID, &m.ReleaseID,
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
	}
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
