package fc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// countingStore records fetches so a test can tell a cache hit from a download.
type countingStore struct {
	objects map[string]string // key -> contents
	fetches map[string]int
}

func newCountingStore() *countingStore {
	return &countingStore{objects: map[string]string{}, fetches: map[string]int{}}
}

func (c *countingStore) PutFile(_ context.Context, key, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c.objects[key] = string(raw)
	return nil
}

func (c *countingStore) GetToFile(_ context.Context, key, path string) error {
	body, ok := c.objects[key]
	if !ok {
		return fmt.Errorf("%w: %s", ErrArtifactMissing, key)
	}
	c.fetches[key]++
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// A suspend reuses one prefix, so its objects change under a stable key.
// Trusting the local copy meant the second wake restored the FIRST snapshot,
// losing everything written in between with no error anywhere.
func TestMutableArtifactsAreAlwaysRefetched(t *testing.T) {
	store := newCountingStore()
	dir := t.TempDir()
	at := Artifacts{Prefix: "machines/m-1/suspend"} // Immutable is false

	store.objects[at.Snap()] = "first"
	if err := fetchAlways(context.Background(), store, at.Snap(), filepath.Join(dir, SnapFile)); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// A second suspend overwrites the object behind the same key.
	store.objects[at.Snap()] = "second"
	if err := fetchAlways(context.Background(), store, at.Snap(), filepath.Join(dir, SnapFile)); err != nil {
		t.Fatalf("second fetch: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, SnapFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("restored %q, want the latest snapshot; the stale local copy shadowed it", got)
	}
	if store.fetches[at.Snap()] != 2 {
		t.Errorf("fetched %d times, want 2", store.fetches[at.Snap()])
	}
}

// An immutable set is written once under its own id, so serving it from cache
// is correct and saves a download.
func TestImmutableArtifactsAreServedFromCache(t *testing.T) {
	store := newCountingStore()
	dir := t.TempDir()
	at := Artifacts{Prefix: "machines/m-1/checkpoints/ck-1", Immutable: true}
	store.objects[at.Snap()] = "v1"

	for i := 0; i < 3; i++ {
		if err := fetchIfAbsent(context.Background(), store, at.Snap(), filepath.Join(dir, SnapFile)); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if store.fetches[at.Snap()] != 1 {
		t.Errorf("fetched %d times, want 1 -- an immutable object should be cached",
			store.fetches[at.Snap()])
	}
}

// Only a genuinely absent object may fall back to the template. A failed fetch
// must not, or a network blip silently resets a machine's disk.
func TestIsMissingDistinguishesAbsentFromFailed(t *testing.T) {
	if !isMissing(fmt.Errorf("wrapped: %w", ErrArtifactMissing)) {
		t.Error("an absent artifact was not recognised")
	}
	if !isMissing(os.ErrNotExist) {
		t.Error("os.ErrNotExist was not recognised")
	}
	for _, err := range []error{
		errors.New("connection reset by peer"),
		errors.New("503 service unavailable"),
		// Previously matched by a substring check on the error text.
		errors.New("dial tcp: lookup s3.example.com: no such host"),
	} {
		if isMissing(err) {
			t.Errorf("a transport failure was treated as a missing object: %v", err)
		}
	}
}
