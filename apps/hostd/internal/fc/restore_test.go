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

// A machine's suspend image is written under ONE key and rewritten on every
// suspend, so a cached copy is a copy of the PREVIOUS suspend. Trusting it
// restores the machine to where it was two suspends ago and loses everything
// since, with no error anywhere -- and only on the host that happens to have
// the stale file.
func TestMutableSnapshotsAreAlwaysRefetched(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snap.bin")

	store := newCountingStore()
	store.objects["k"] = "second"
	if err := os.WriteFile(dest, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fetchAlways(context.Background(), store, "k", dest); err != nil {
		t.Fatalf("fetchAlways: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("read back %q; a stale local copy was used", got)
	}
}

// A checkpoint is written once under its own id and never rewritten, so its
// local copy is always current and re-downloading it wastes a restore.
func TestImmutableSnapshotsAreServedFromCache(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "snap.bin")

	store := newCountingStore()
	store.objects["k"] = "remote"
	if err := os.WriteFile(dest, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fetchIfAbsent(context.Background(), store, "k", dest); err != nil {
		t.Fatalf("fetchIfAbsent: %v", err)
	}
	if store.fetches["k"] != 0 {
		t.Errorf("made %d fetches for an artifact already on disk", store.fetches["k"])
	}
}

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
