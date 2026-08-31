package machines

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// deletingStore records what was removed from object storage.
type deletingStore struct {
	deleted []string
	fail    bool
}

func (s *deletingStore) PutFile(context.Context, string, string) error   { return nil }
func (s *deletingStore) GetToFile(context.Context, string, string) error { return nil }

func (s *deletingStore) Delete(_ context.Context, key string) error {
	if s.fail {
		return errors.New("deletingStore: injected failure")
	}
	s.deleted = append(s.deleted, key)
	return nil
}

// Every suspend writes a fresh memory build, so the one it replaces has to go.
// Left behind, each suspend leaks a full memory diff -- the largest objects the
// system produces -- for a machine that may wake and sleep on a schedule
// forever.
func TestDiscardBuildsRemovesBothObjects(t *testing.T) {
	store := &deletingStore{}
	m := &Manager{opts: Options{Chunks: store}}

	m.discardBuilds(context.Background(), "build-a", "build-b")

	got := append([]string(nil), store.deleted...)
	sort.Strings(got)
	want := []string{
		"build-a/data", "build-a/header",
		"build-b/data", "build-b/header",
	}
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("deleted %v, want %v", got, want)
			break
		}
	}
}

// A machine that wrote nothing to disk has no rootfs build, and its empty id
// must not be turned into a key. Deleting "/header" would at best do nothing
// and at worst remove something else.
func TestDiscardBuildsIgnoresAbsentIDs(t *testing.T) {
	store := &deletingStore{}
	m := &Manager{opts: Options{Chunks: store}}

	m.discardBuilds(context.Background(), "", "build-a", "")

	for _, key := range store.deleted {
		if key == "/header" || key == "/data" {
			t.Errorf("an empty build id became the key %q", key)
		}
	}
	if len(store.deleted) != 2 {
		t.Errorf("deleted %v, want only build-a's two objects", store.deleted)
	}
}

// Storage cleanup must never be able to fail an operation: losing a machine
// because its predecessor's bytes could not be deleted is a far worse outcome
// than paying for them.
func TestDiscardBuildsToleratesFailure(t *testing.T) {
	m := &Manager{opts: Options{Chunks: &deletingStore{fail: true}}}
	m.discardBuilds(context.Background(), "build-a") // must not panic
}

// A host with no object storage configured has nothing to delete from, and
// must not fall over trying.
func TestDiscardBuildsWithoutAStore(t *testing.T) {
	m := &Manager{opts: Options{}}
	m.discardBuilds(context.Background(), "build-a")
}
