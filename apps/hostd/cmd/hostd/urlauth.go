package main

import (
	"context"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Who may reach a URL, answered on the router's hot path.
//
// # The bug this exists for
//
// The gate read the subscription cache and nothing else, and an absent row
// reads as "public". On the rig a machine created with url_auth=org answered an
// anonymous request: the cache had not materialized the row, so a URL somebody
// had deliberately gated was open to anyone, silently, with nothing
// distinguishing it from a URL meant to be public.
//
// That is the permissive direction, which is the one you cannot take back.
//
// # Why not just query the store
//
// Because this is every HTTP request to every workload, and an absent row is
// the COMMON case: url_auth is opt-in, so most machines have none. Falling
// through to the store on every miss would put a query on the hot path for
// every request to every ungated machine, which is what the original design
// deliberately avoided.
//
// So a miss is answered from the store ONCE and remembered for a few seconds.
// The hot path stays a map read; a gate written a moment ago takes effect
// within the memo's life rather than never; and the store is consulted at most
// once per machine per interval however much traffic arrives.
//
// # Why "org" from the cache is trusted outright
//
// Nothing downgrades a URL to public except an explicit write, so a stale
// "org" costs a caller one 401 they can fix by sending the key they already
// have. Only the miss is dangerous.

// urlAuthMemoTTL is how long a store answer is reused.
//
// Short enough that gating a URL takes effect while somebody is still watching
// the command that did it, and long enough that a busy machine costs one query
// rather than thousands.
const urlAuthMemoTTL = 5 * time.Second

type urlAuthAnswer struct {
	mode string
	at   time.Time
}

// urlAuthGate answers the router's question, cache first and store on a miss.
type urlAuthGate struct {
	cache func(id string) string
	store state.Store

	mu   sync.Mutex
	memo map[string]urlAuthAnswer
}

func newURLAuthGate(cache func(id string) string, store state.Store) *urlAuthGate {
	return &urlAuthGate{cache: cache, store: store, memo: map[string]urlAuthAnswer{}}
}

// Forget drops a memoised answer, because this host just changed it.
//
// Without this the memo outlives the write that invalidates it. A URL PATCHed
// back to public went on answering 401 for the rest of the memo's life: the
// cache had not caught up either, so the miss path was still returning the
// "org" it had read a moment earlier. The write and the read are on the same
// host, so the host that changes a mode can simply say so.
func (g *urlAuthGate) Forget(id string) {
	g.mu.Lock()
	delete(g.memo, id)
	g.mu.Unlock()
}

func (g *urlAuthGate) Mode(ctx context.Context, id string) string {
	// ANY answer the cache has is authoritative, public included.
	//
	// Only the MISS was ever dangerous. Falling through on an explicit public
	// was the bug's mirror image: a URL the cache correctly knew to be open
	// was answered from a memo that still held the mode it used to have.
	if g.cache != nil {
		if mode := g.cache(id); mode != "" {
			return mode
		}
	}
	if g.store == nil {
		return api.URLAuthPublic
	}

	g.mu.Lock()
	answer, ok := g.memo[id]
	g.mu.Unlock()
	if ok && time.Since(answer.at) < urlAuthMemoTTL {
		return answer.mode
	}

	mode := api.URLAuthPublic
	if u, err := g.store.GetURLAuth(ctx, id); err == nil && u != nil && u.Mode != "" {
		mode = u.Mode
	}

	g.mu.Lock()
	// Bounded by clearing rather than by evicting one entry at a time. The keys
	// are machine and service ids, so the map grows with the fleet and not with
	// traffic; clearing it costs one rebuild of at most a few seconds of
	// answers, which is cheaper than tracking an order nobody reads.
	if len(g.memo) > 4096 {
		g.memo = map[string]urlAuthAnswer{}
	}
	g.memo[id] = urlAuthAnswer{mode: mode, at: time.Now()}
	g.mu.Unlock()
	return mode
}
