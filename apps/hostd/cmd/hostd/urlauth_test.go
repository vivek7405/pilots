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
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthOrg {
		t.Fatalf("a gated URL was served as %q", mode)
	}
}

// The reason the store is not simply consulted every time: this is the router's
// hot path, and a machine with no url_auth row at all is the common case.
func TestRepeatedMissesAskTheStoreOnce(t *testing.T) {
	store := &countingStore{}
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, store)

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
	g := newURLAuthGate(func(string) (string, bool) { return api.URLAuthOrg, true }, store)

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
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, &countingStore{})
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
	g := newURLAuthGate(func(string) (string, bool) { return api.URLAuthPublic, true }, store)

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
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, store)

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
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, &countingStore{})
	g.Forget("never-seen")
}

// The cache's "public" for an object it has never seen is not an answer.
//
// # The bug this exists for, twice
//
// corrosion.Cache.URLAuth returns the literal string "public" for an id it has
// no row for, which is indistinguishable from an object somebody deliberately
// made public. Every version of this gate that read that one string got one of
// the two directions wrong:
//
//   - Trusting it served every gated URL to anyone until the subscription
//     delivered the row. That is the security bug this gate was written for.
//   - Distrusting it refused every URL the cache correctly knew to be public,
//     because the fallback memo still held the mode it used to have. That is
//     what the fix caused, and the battery caught it as "public again should
//     not be gated, got 401".
//
// Neither is fixable from the mode alone. The cache has to be able to say it
// does not know, which is what URLAuthKnown's second return is for.
func TestAMissDressedAsPublicIsStillAMiss(t *testing.T) {
	store := &countingStore{mode: api.URLAuthOrg}
	// Exactly what the real cache does for an id it has never seen.
	g := newURLAuthGate(func(string) (string, bool) { return "public", false }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthOrg {
		t.Fatalf("a gated URL was served as %q. The cache said \"public\" because "+
			"it had no row, and that was read as an answer", mode)
	}
	if store.asked == 0 {
		t.Error("the store was never consulted, so the miss was taken at face value")
	}
}

// And a KNOWN public still costs nothing, which is the whole reason the second
// return exists rather than simply always reading the store.
func TestAKnownPublicIsAnsweredFromTheCache(t *testing.T) {
	store := &countingStore{mode: api.URLAuthOrg}
	g := newURLAuthGate(func(string) (string, bool) { return api.URLAuthPublic, true }, store)

	if mode := g.Mode(context.Background(), "m1"); mode != api.URLAuthPublic {
		t.Fatalf("got %q, want the cache's public", mode)
	}
	if store.asked != 0 {
		t.Errorf("a known answer cost %d store reads", store.asked)
	}
}
