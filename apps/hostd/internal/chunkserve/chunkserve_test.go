package chunkserve

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// fakeStore stands in for the bucket, and records what was actually asked of
// it, so a test can assert that a refused request never reached storage.
type fakeStore struct {
	objects map[string][]byte
	gets    []string
}

func (f *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	f.gets = append(f.gets, key)
	raw, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such object: " + key)
	}
	return raw, nil
}

func (f *fakeStore) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	f.gets = append(f.gets, key)
	raw, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such object: " + key)
	}
	if off >= int64(len(raw)) {
		// What a zero-length data object does to every range against it, and
		// the case the client has to be able to tell apart: it means "these
		// blocks are zeros", not "this read failed".
		return nil, block.ErrRangeNotSatisfiable
	}
	end := off + length
	if length == 0 || end > int64(len(raw)) {
		end = int64(len(raw))
	}
	return raw[off:end], nil
}

func newPair(t *testing.T, allowed ...string) (*Server, *Client, *fakeStore) {
	t.Helper()
	store := &fakeStore{objects: map[string][]byte{
		"build-mine/header": []byte("mine-header"),
		"build-mine/data":   []byte("0123456789"),
		"build-other/data":  []byte("another tenant's bytes"),
	}}
	path := filepath.Join(t.TempDir(), SocketName)
	srv, err := New("m_1", path, store, allowed)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	c := Dial(path)
	t.Cleanup(func() { c.Close() })
	return srv, c, store
}

func TestAHandlerReadsItsOwnBuild(t *testing.T) {
	_, c, _ := newPair(t, "build-mine")

	got, err := c.Get(context.Background(), "build-mine/header")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "mine-header" {
		t.Errorf("Get returned %q", got)
	}

	part, err := c.GetRange(context.Background(), "build-mine/data", 2, 3)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if string(part) != "234" {
		t.Errorf("GetRange returned %q, want 234", part)
	}
}

// The assertion the whole package exists for. A handler that asked for another
// machine's build must be refused, and the request must never reach storage:
// a proxy that fetched it and then said no would be a credential with extra
// steps.
func TestAForeignBuildIsRefusedAndNeverReachesStorage(t *testing.T) {
	srv, c, store := newPair(t, "build-mine")

	_, err := c.Get(context.Background(), "build-other/data")
	if err == nil {
		t.Fatal("a foreign build was served")
	}
	if !strings.Contains(err.Error(), "may not read") {
		t.Errorf("error is %v, want a refusal naming the key", err)
	}
	for _, k := range store.gets {
		if strings.HasPrefix(k, "build-other") {
			t.Errorf("the refused key still reached storage: %v", store.gets)
		}
	}
	if srv.Refused() != 1 {
		t.Errorf("refused count is %d, want 1", srv.Refused())
	}

	// And the connection survives it: one refusal must not wedge the handler's
	// own reads.
	if _, err := c.Get(context.Background(), "build-mine/header"); err != nil {
		t.Errorf("a legitimate read after a refusal failed: %v", err)
	}
}

func TestATraversalIsRefused(t *testing.T) {
	srv, c, _ := newPair(t, "build-mine")
	for _, key := range []string{
		"build-mine/../build-other/data",
		"../../etc/passwd",
		"",
	} {
		if _, err := c.Get(context.Background(), key); err == nil {
			t.Errorf("key %q was served", key)
		}
	}
	if srv.Refused() != 3 {
		t.Errorf("refused count is %d, want 3", srv.Refused())
	}
}

// A zero-length data object answers every range with 416, and the block layer
// reads that as "these blocks are zeros". If the socket flattened it into a
// generic error, every wake of a fully-elided build would fail.
func TestRangeNotSatisfiableSurvivesTheWire(t *testing.T) {
	_, c, _ := newPair(t, "build-mine")

	_, err := c.GetRange(context.Background(), "build-mine/data", 999, 10)
	if !errors.Is(err, block.ErrRangeNotSatisfiable) {
		t.Errorf("err = %v, want it to satisfy block.ErrRangeNotSatisfiable", err)
	}
}

// A build id learned after the machine started, which is what a late-resolving
// rehydrate does.
func TestAllowAddsABuildAfterTheFact(t *testing.T) {
	srv, c, _ := newPair(t, "build-mine")

	if _, err := c.Get(context.Background(), "build-other/data"); err == nil {
		t.Fatal("the build was readable before it was allowed")
	}
	srv.Allow("build-other")
	if _, err := c.Get(context.Background(), "build-other/data"); err != nil {
		t.Errorf("the build is still refused after Allow: %v", err)
	}
}

// The handler outlives hostd by design, so a restart that takes the socket
// with it must cost one failed read rather than a wedged machine.
func TestTheClientReconnectsAfterTheServerRestarts(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{"build-mine/header": []byte("mine-header")}}
	path := filepath.Join(t.TempDir(), SocketName)

	srv, err := New("m_1", path, store, []string{"build-mine"})
	if err != nil {
		t.Fatal(err)
	}
	c := Dial(path)
	defer c.Close()
	if _, err := c.Get(context.Background(), "build-mine/header"); err != nil {
		t.Fatal(err)
	}

	srv.Close()
	srv2, err := New("m_1", path, store, []string{"build-mine"})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer srv2.Close()

	if _, err := c.Get(context.Background(), "build-mine/header"); err != nil {
		t.Errorf("the client did not reconnect after a restart: %v", err)
	}
}
