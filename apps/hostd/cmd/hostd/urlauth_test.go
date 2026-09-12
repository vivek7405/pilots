package main

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// countingStore answers GetURLAuth and counts how often it was asked.
//
// Only that one method is real. Everything else is the embedded interface, so a
// call to any of it panics loudly rather than returning a zero value that would
// let a wrong code path pass quietly.
type countingStore struct {
	state.Store
	mode  string
	asked int
}

func (s *countingStore) GetURLAuth(_ context.Context, _ string) (*state.URLAuth, error) {
	s.asked++
	if s.mode == "" {
		return nil, nil
	}
	return &state.URLAuth{Mode: s.mode}, nil
}

// The bug: a machine gated to its org answered anonymous requests because the
// cache had no row for it yet and an absent row reads as public.
func TestACacheMissFindsTheGateInTheStore(t *testing.T) {
	store := &countingStore{mode: api.URLAuthOrg}
	g := newURLAuthGate(func(string) string { return "" }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthOrg {
		t.Fatalf("a gated URL was served as %q", mode)
	}
}

// The reason the store is not simply consulted every time: this is the router's
// hot path, and a machine with no url_auth row at all is the common case.
func TestRepeatedMissesAskTheStoreOnce(t *testing.T) {
	store := &countingStore{}
	g := newURLAuthGate(func(string) string { return "" }, store)

	for range 50 {
		if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthPublic {
			t.Fatalf("an ungated URL came back as %q", mode)
		}
	}
	if store.asked != 1 {
		t.Fatalf("the hot path hit the store %d times, want 1", store.asked)
	}
}

// A gated answer from the cache is authoritative, so it must not cost a query.
func TestAGatedCacheHitDoesNotAskTheStore(t *testing.T) {
	store := &countingStore{}
	g := newURLAuthGate(func(string) string { return api.URLAuthOrg }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthOrg {
		t.Fatalf("got %q, want %q", mode, api.URLAuthOrg)
	}
	if store.asked != 0 {
		t.Fatalf("a cache hit still asked the store %d times", store.asked)
	}
}

// A store that errors or has nothing must leave the URL public rather than
// locking out a URL that was never gated.
func TestNothingAnywhereMeansPublic(t *testing.T) {
	g := newURLAuthGate(func(string) string { return "" }, &countingStore{})
	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthPublic {
		t.Fatalf("got %q, want %q", mode, api.URLAuthPublic)
	}
}

// An explicit public from the cache is an answer, not a miss.
//
// # The bug this exists for, which the FIRST version of this fix caused
//
// The gate read "anything that is not org is not authoritative", so an
// explicit public fell through to the store-backed memo -- which, on a URL
// that had just been changed from org to public, still held org. The URL went
// on answering 401 for the life of the memo. The e2e battery caught it as
// "public again should not be gated, got 401".
//
// Only the MISS was ever dangerous. Any answer the cache actually has is the
// answer.
func TestAnExplicitPublicFromTheCacheIsTrusted(t *testing.T) {
	store := &countingStore{mode: api.URLAuthOrg} // what the memo would have held
	g := newURLAuthGate(func(string) string { return api.URLAuthPublic }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthPublic {
		t.Fatalf("a URL the cache knows to be public came back as %q", mode)
	}
	if store.asked != 0 {
		t.Errorf("an explicit answer still cost %d store reads", store.asked)
	}
}

// Changing the mode drops what was memoised about it.
//
// The write and the read are on the same host, so the host that changes a mode
// can say so rather than waiting for its own memo to expire. Without this a
// URL opened to the public keeps refusing anonymous callers for as long as the
// memo lives, which is the failure above arriving by a different route.
func TestForgettingAModeDropsTheMemo(t *testing.T) {
	store := &countingStore{mode: api.URLAuthOrg}
	// A cache that knows nothing, so every answer comes from the store.
	g := newURLAuthGate(func(string) string { return "" }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthOrg {
		t.Fatalf("got %q, want the store's org", mode)
	}
	if store.asked != 1 {
		t.Fatalf("the store was asked %d times, want 1", store.asked)
	}

	// Memoised: a second read costs nothing.
	g.Mode(context.Background(), "m1")
	if store.asked != 1 {
		t.Fatalf("the memo did not hold; the store was asked %d times", store.asked)
	}

	// The mode changes, and this host says so.
	store.mode = api.URLAuthPublic
	g.Forget("m1")

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthPublic {
		t.Fatalf("after Forget the gate still answered %q; a URL opened to the "+
			"public goes on refusing anonymous callers", mode)
	}
}

// Forgetting something never memoised is not an error, because most writes
// are to objects nothing has asked about yet.
func TestForgettingAnUnknownIDIsHarmless(t *testing.T) {
	g := newURLAuthGate(func(string) string { return "" }, &countingStore{})
	g.Forget("never-seen")
}
