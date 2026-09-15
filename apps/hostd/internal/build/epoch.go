package build

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Resetting an org's build cache, fleet-wide, without a coordinator.
//
// "My builds are wrong, clear the cache" is the oldest request a build service
// gets, and fly's builder page has a Reset button for exactly it. The hard
// part here is that the cache is not in one place: every host that has built
// for this org holds a copy on its own NVMe, mirrored from one bucket prefix.
// Telling them all to drop it would need a message to every host, which is a
// coordinator, which rule 1 forbids.
//
// So a reset is a WRITE rather than a broadcast. One small object holds an
// integer, the epoch; the reset increments it; the epoch is part of the key
// every host mirrors to and from. A host with a warm local directory compares
// the epoch it pulled at against the bucket's before taking its early return,
// and re-pulls when they differ. Nothing is pushed, nothing is subscribed to,
// and a host that was powered off during the reset finds out at its next
// build.
//
// Monotonic and written once per reset, so it is single-writer safe under
// invariant 1 in the way the invariant's own text describes: a row that is
// only ever advanced has nothing for a merge to corrupt. Two resets racing
// produce one increment rather than two, which costs an extra cold build and
// no correctness.

// epochFile is where a local cache directory records the epoch it was pulled
// at. Inside the directory, so removing the directory removes the record with
// it and a half-deleted cache cannot claim to be current.
const epochFile = ".epoch"

// epochKey is the object holding an org's current epoch.
func epochKey(orgID string) string {
	return "orgs/" + sanitizeKey(orgID) + "/epoch"
}

// cacheKeyPrefixAt is where an org's cache for one Dockerfile lives at a given
// epoch. Epoch 0 keeps the original layout, so a fleet that has never reset
// finds exactly the objects it already wrote.
func cacheKeyPrefixAt(orgID, cacheName string, epoch int) string {
	if epoch <= 0 {
		return cacheKeyPrefix(orgID, cacheName)
	}
	return "orgs/" + sanitizeKey(orgID) + "/e" + strconv.Itoa(epoch) + "/" + cacheName + "/"
}

// readEpoch reads an org's current epoch from the bucket.
//
// An absent object is epoch 0, which is every org that has never reset. An
// unreadable one is also 0 and is logged: a build must not fail because the
// cache bookkeeping could not be read, and a wrong-way answer here costs a
// cold build rather than a wrong one.
func (b *Builder) readEpoch(ctx context.Context, orgID string) int {
	if b.opts.CacheStore == nil {
		return 0
	}
	tmp, err := os.CreateTemp("", "pilots-epoch-")
	if err != nil {
		return 0
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	if err := b.opts.CacheStore.GetToFile(ctx, epochKey(orgID), path); err != nil {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 {
		slog.Warn("the build cache epoch is unreadable; treating it as 0",
			"org", orgID, "value", strings.TrimSpace(string(raw)))
		return 0
	}
	return n
}

// BumpEpoch advances an org's cache epoch and returns the new value.
//
// The old epoch's objects are left for the bucket's own lifecycle rather than
// deleted here: a delete of every object under a prefix is a long operation
// that a request should not hold, and an object nothing will ever name again
// costs storage rather than correctness. What matters for the reset is that
// the epoch moved.
func (b *Builder) BumpEpoch(ctx context.Context, orgID string) (int, error) {
	next := b.readEpoch(ctx, orgID) + 1

	tmp, err := os.CreateTemp("", "pilots-epoch-")
	if err != nil {
		return 0, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(strconv.Itoa(next)); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if b.opts.CacheStore == nil {
		return next, nil
	}
	if err := b.opts.CacheStore.PutFile(ctx, epochKey(orgID), path); err != nil {
		return 0, err
	}
	return next, nil
}

// readLocalEpoch reports the epoch a local cache directory was pulled at.
// A directory with no record predates this and reads as 0.
func readLocalEpoch(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, epochFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// writeLocalEpoch stamps a freshly pulled cache directory.
func writeLocalEpoch(dir string, epoch int) {
	if err := os.WriteFile(filepath.Join(dir, epochFile),
		[]byte(strconv.Itoa(epoch)), 0o644); err != nil {
		// Not fatal: the cost is that this host re-pulls once more than it
		// needed to, which is the safe direction.
		slog.Warn("could not record the cache epoch", "dir", dir, "err", err)
	}
}
