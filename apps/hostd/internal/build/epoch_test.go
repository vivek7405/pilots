package build

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/s3"
)

// memCacheStore is a CacheStore backed by a map, so the epoch mechanism can be
// tested without a bucket.
type memCacheStore struct {
	objects map[string][]byte
	puts    int
}

func newMemCacheStore() *memCacheStore {
	return &memCacheStore{objects: map[string][]byte{}}
}

func (m *memCacheStore) PutFile(_ context.Context, key, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m.objects[key] = raw
	m.puts++
	return nil
}

func (m *memCacheStore) GetToFile(_ context.Context, key, path string) error {
	raw, ok := m.objects[key]
	if !ok {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func (m *memCacheStore) List(_ context.Context, prefix string) ([]s3.ObjectInfo, error) {
	var out []s3.ObjectInfo
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3.ObjectInfo{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *memCacheStore) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

func newEpochBuilder(t *testing.T) (*Builder, *memCacheStore) {
	t.Helper()
	store := newMemCacheStore()
	return &Builder{opts: Options{
		BuildctlBin: "buildctl",
		CacheDir:    t.TempDir(),
		CacheStore:  store,
	}}, store
}

func TestEpochStartsAtZeroAndAdvances(t *testing.T) {
	b, _ := newEpochBuilder(t)
	ctx := context.Background()

	if got := b.readEpoch(ctx, "org_1"); got != 0 {
		t.Errorf("a fresh org reads epoch %d, want 0", got)
	}
	next, err := b.BumpEpoch(ctx, "org_1")
	if err != nil {
		t.Fatalf("BumpEpoch: %v", err)
	}
	if next != 1 || b.readEpoch(ctx, "org_1") != 1 {
		t.Errorf("after one reset the epoch is %d, want 1", next)
	}
	if next, _ = b.BumpEpoch(ctx, "org_1"); next != 2 {
		t.Errorf("after two resets the epoch is %d, want 2", next)
	}
}

// One org's reset must not reach another's cache: that would cost every host
// in the fleet a cold build for a tenant who asked for nothing.
func TestEpochIsPerOrg(t *testing.T) {
	b, _ := newEpochBuilder(t)
	ctx := context.Background()

	if _, err := b.BumpEpoch(ctx, "org_1"); err != nil {
		t.Fatal(err)
	}
	if got := b.readEpoch(ctx, "org_2"); got != 0 {
		t.Errorf("org_2's epoch is %d after org_1 reset, want 0", got)
	}
}

// Epoch 0 keeps the layout every existing object was written under, so a fleet
// that has never reset finds exactly what it already has.
func TestEpochZeroKeepsTheOriginalKeyLayout(t *testing.T) {
	if got := cacheKeyPrefixAt("org_1", "df-abc", 0); got != cacheKeyPrefix("org_1", "df-abc") {
		t.Errorf("epoch 0 prefix is %q, want the original %q", got, cacheKeyPrefix("org_1", "df-abc"))
	}
	if got := cacheKeyPrefixAt("org_1", "df-abc", 2); got == cacheKeyPrefix("org_1", "df-abc") {
		t.Errorf("epoch 2 shares the original prefix %q, so a reset would find the old objects", got)
	}
}

// The mechanism itself. A host with a warm local cache from before a reset
// must drop it, or Reset is a no-op on every host that already had a copy,
// which is every host that has ever built for the org.
func TestPullCacheDropsAWarmCopyFromBeforeAReset(t *testing.T) {
	b, store := newEpochBuilder(t)
	ctx := context.Background()
	dir := b.cacheDir("org_1", "df-abc")

	// A warm cache, pulled at epoch 0.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{"old":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLocalEpoch(dir, 0)

	// Nothing changed: the warm copy is kept.
	b.pullCache(ctx, dir, "org_1", "df-abc")
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Fatalf("a warm cache at the current epoch was dropped: %v", err)
	}

	// After a reset it is not.
	if _, err := b.BumpEpoch(ctx, "org_1"); err != nil {
		t.Fatal(err)
	}
	b.pullCache(ctx, dir, "org_1", "df-abc")
	if _, err := os.Stat(filepath.Join(dir, "index.json")); !os.IsNotExist(err) {
		t.Errorf("the stale cache survived a reset (err %v); Reset would be a no-op here", err)
	}
	_ = store
}

// A directory written before epochs existed reads as 0, which is what every
// cache on a running fleet is at the moment this ships.
func TestALocalCacheWithNoRecordReadsAsEpochZero(t *testing.T) {
	dir := t.TempDir()
	if got := readLocalEpoch(dir); got != 0 {
		t.Errorf("a directory with no record reads epoch %d, want 0", got)
	}
	writeLocalEpoch(dir, 3)
	if got := readLocalEpoch(dir); got != 3 {
		t.Errorf("after stamping, the directory reads epoch %d, want 3", got)
	}
	if err := os.WriteFile(filepath.Join(dir, epochFile), []byte("nonsense"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLocalEpoch(dir); got != 0 {
		t.Errorf("an unreadable record reads epoch %d, want 0", got)
	}
}

// A build must never fail over cache bookkeeping. An unreadable epoch object
// costs a cold build, which is the safe direction.
func TestAnUnreadableEpochReadsAsZero(t *testing.T) {
	b, store := newEpochBuilder(t)
	store.objects[epochKey("org_1")] = []byte("not a number")
	if got := b.readEpoch(context.Background(), "org_1"); got != 0 {
		t.Errorf("an unreadable epoch read as %d, want 0", got)
	}
}
