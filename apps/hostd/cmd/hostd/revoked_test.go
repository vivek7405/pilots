package main

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// emptyCache is a subscription cache that knows nothing, which is exactly the
// state the rig's was in when a revoked key went on working.
type emptyCache struct{ revoked map[string]bool }

func (e emptyCache) OrgOf(string) (string, bool) { return "", false }
func (e emptyCache) Revoked(hash string) bool    { return e.revoked[hash] }
func (e emptyCache) KeyLimits(string) (state.APIKeyLimits, bool) {
	return state.APIKeyLimits{}, false
}

// A revoked key must stop working, and a cache that has not caught up must not
// be what decides it has not been revoked.
//
// On the rig it was. A key was minted, revoked, and went on authenticating on
// every request for as long as anybody cared to try, before and after a hostd
// restart. The tombstone was written and present in the table on every host;
// the cache the auth path reads did not have it, and nothing anywhere said so.
//
// These tests pin the asymmetry that makes the fix correct rather than merely
// safer: a cache HIT is authoritative, because a revocation is a tombstone that
// only ever appears and un-revoking is minting a new key. A cache MISS decides
// nothing, because the cost of being wrong in that direction is a credential
// somebody believes is dead.

// revokedStore is the local store half, with nothing in it but a set of hashes.
type revokedStore struct {
	state.Store
	hashes map[string]bool
	asked  int
}

func (r *revokedStore) IsRevoked(_ context.Context, hash string) (bool, error) {
	r.asked++
	return r.hashes[hash], nil
}

func TestACacheMissFallsThroughToTheStore(t *testing.T) {
	store := &revokedStore{hashes: map[string]bool{"dead": true}}
	// An empty cache, which is exactly the state the rig was in.
	tenancy := cachedTenancy{cache: emptyCache{}, store: store}

	revoked, err := tenancy.Revoked(context.Background(), "dead")
	if err != nil {
		t.Fatalf("Revoked: %v", err)
	}
	if !revoked {
		t.Error("a key the store knows is revoked authenticated, because the cache " +
			"had not caught up: this is the bug, and it is silent and total")
	}
	if store.asked == 0 {
		t.Error("the store was never consulted on a cache miss")
	}
}

func TestAKeyNobodyRevokedIsNotRevoked(t *testing.T) {
	store := &revokedStore{hashes: map[string]bool{"dead": true}}
	tenancy := cachedTenancy{cache: emptyCache{}, store: store}

	revoked, err := tenancy.Revoked(context.Background(), "alive")
	if err != nil {
		t.Fatalf("Revoked: %v", err)
	}
	if revoked {
		t.Error("a key nobody revoked was refused")
	}
}

// The hit has to short-circuit, or the fallback is not a fallback: it is a
// store query on every authenticated request whether or not the cache knew.
func TestACacheHitDoesNotAskTheStore(t *testing.T) {
	store := &revokedStore{hashes: map[string]bool{}}
	tenancy := cachedTenancy{
		cache: emptyCache{revoked: map[string]bool{"dead": true}},
		store: store,
	}
	revoked, err := tenancy.Revoked(context.Background(), "dead")
	if err != nil {
		t.Fatalf("Revoked: %v", err)
	}
	if !revoked {
		t.Fatal("a cached revocation was not honoured")
	}
	if store.asked != 0 {
		t.Errorf("the store was asked %d times for a hash the cache already had", store.asked)
	}
}
