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
