package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layout writes a minimal OCI cache layout, which is what the exporter leaves
// behind and what swapExported looks for.
func layout(t *testing.T, dir, blob string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"schemaVersion":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256-x"), []byte(blob), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A pushed cache goes under the epoch this host pulled at.
//
// pushCache read the epoch AFTER swapExported, and swapExported replaces the
// pulled directory with the exporter's fresh output, which carries no stamp.
// So the read answered 0 every time and the cache was mirrored under epoch 0
// while the fleet was at epoch N: no other host ever found it.
//
// Moving the read back after the swap reds this with an "orgs/org_1/..." key
// instead of an "orgs/org_1/e3/..." one.
func TestACacheIsPushedUnderTheEpochItWasPulledAt(t *testing.T) {
	b, store := newEpochBuilder(t)
	dir := filepath.Join(t.TempDir(), "cache")
	layout(t, dir, "old")
	writeLocalEpoch(dir, 3)
	// What a build leaves: a freshly exported layout beside the pulled one.
	layout(t, dir+exportSuffix, "new")

	b.pushCache(context.Background(), dir, "org_1", "app")

	var keys []string
	for k := range store.objects {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		t.Fatal("nothing was mirrored")
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "orgs/org_1/e3/app/") {
			t.Errorf("mirrored under %q, want the e3 prefix this host pulled at; "+
				"every other host at epoch 3 looks there and finds nothing", k)
		}
	}
}

// The swapped-in generation keeps the epoch it was built from.
//
// Without the re-stamp the directory is left unstamped, so the next pullCache
// compares 0 against the fleet's epoch, re-pulls a cache it already has, and
// the push after that unstamps it again -- and a `pilot builder reset`, whose
// whole mechanism is bumping the epoch, does not reset the cache but switches
// it off for good.
func TestTheSwappedCacheKeepsItsEpochStamp(t *testing.T) {
	b, _ := newEpochBuilder(t)
	dir := filepath.Join(t.TempDir(), "cache")
	layout(t, dir, "old")
	writeLocalEpoch(dir, 7)
	layout(t, dir+exportSuffix, "new")

	b.pushCache(context.Background(), dir, "org_1", "app")

	if got := readLocalEpoch(dir); got != 7 {
		t.Errorf("the rotated cache reads epoch %d, want 7: the next pull will "+
			"discard it and the one after that will discard the replacement", got)
	}
	// And it really did rotate, so this is not passing because nothing moved.
	blob, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256-x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "new" {
		t.Errorf("blob = %q, want the freshly exported generation", blob)
	}
}

// This host's own stamp is not mirrored. Every host writes its own after a
// pull, so a copy in the bucket is a value that is true of nobody.
func TestTheEpochStampIsNotMirrored(t *testing.T) {
	b, store := newEpochBuilder(t)
	dir := filepath.Join(t.TempDir(), "cache")
	layout(t, dir, "old")
	writeLocalEpoch(dir, 2)
	layout(t, dir+exportSuffix, "new")

	b.pushCache(context.Background(), dir, "org_1", "app")

	for k := range store.objects {
		if strings.HasSuffix(k, epochFile) {
			t.Errorf("the epoch stamp was mirrored as %q", k)
		}
	}
}

// A build that exported nothing leaves the previous generation, and its stamp,
// exactly where they were.
func TestACacheThatDidNotRotateKeepsItsStamp(t *testing.T) {
	b, _ := newEpochBuilder(t)
	dir := filepath.Join(t.TempDir(), "cache")
	layout(t, dir, "old")
	writeLocalEpoch(dir, 5)
	// No export directory at all: every step was a cache hit.

	b.pushCache(context.Background(), dir, "org_1", "app")

	if got := readLocalEpoch(dir); got != 5 {
		t.Errorf("epoch = %d, want 5", got)
	}
	blob, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256-x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "old" {
		t.Errorf("blob = %q, want the generation that was already there", blob)
	}
}
