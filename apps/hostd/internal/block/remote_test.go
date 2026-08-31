package block

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// fakeStore serves builds from memory and counts requests, so a test can tell
// a cache hit from a fetch.
type fakeStore struct {
	objects map[string][]byte
	gets    map[string]int
	ranges  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, gets: map[string]int{}}
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake store: no such key %s", key)
	}
	s.gets[key]++
	return body, nil
}

func (s *fakeStore) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake store: no such key %s", key)
	}
	s.ranges++
	// Real object storage answers 416 when the range starts past the end,
	// which is exactly what a fully-elided build produces.
	if off >= int64(len(body)) {
		return nil, fmt.Errorf("fake store: %w", ErrRangeNotSatisfiable)
	}
	end := off + length
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	return body[off:end], nil
}

// publish chunkifies an input and uploads the result to the fake store.
func publish(t *testing.T, s *fakeStore, dir, in string, id uuid.UUID, parentDir string) {
	t.Helper()
	if _, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: in, OutDir: dir, BuildID: id, BlockSize: testBlock, ParentDir: parentDir,
	}); err != nil {
		t.Fatalf("Chunkify: %v", err)
	}
	for _, name := range []string{"header", "data"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		s.objects[id.String()+"/"+name] = raw
	}
}

// The predecessor's diff-chain test hand-stitched the merged view from two
// byte slices and never called Slice, so the parent dispatch -- the thing that
// breaks a wake -- had no coverage at all. This drives the real path.
func TestRemoteBuildDispatchesToParent(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	// Parent: four distinct blocks.
	parentIn := writeBlocks(t, dir, 1, 2, 3, 4)
	parentID := uuid.New()
	publish(t, store, filepath.Join(dir, "parent"), parentIn, parentID, "")

	// Child: block 2 changed, the rest identical and therefore parent-served.
	childPath := filepath.Join(dir, "child.bin")
	var buf bytes.Buffer
	for _, f := range []byte{1, 2, 9, 4} {
		buf.Write(bytes.Repeat([]byte{f}, int(testBlock)))
	}
	if err := os.WriteFile(childPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	childID := uuid.New()
	publish(t, store, filepath.Join(dir, "child"), childPath, childID,
		filepath.Join(dir, "parent"))

	cacheRoot := filepath.Join(dir, "cache")
	parent, err := OpenRemoteBuild(ctx, store, parentID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(parent): %v", err)
	}
	defer parent.Close()

	child, err := OpenRemoteBuild(ctx, store, childID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(child): %v", err)
	}
	defer child.Close()
	child.SetParent(parent)

	got := readWhole(t, child)
	if !bytes.Equal(got, buf.Bytes()) {
		t.Errorf("merged view differs at byte %d", firstDiff(got, buf.Bytes()))
	}
}

// Without a parent attached, a range the diff did not store cannot be served.
// Failing loudly beats returning zeros that look like real data.
func TestRemoteBuildWithoutParentFailsClearly(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	parentIn := writeBlocks(t, dir, 1, 2)
	parentID := uuid.New()
	publish(t, store, filepath.Join(dir, "parent"), parentIn, parentID, "")

	childID := uuid.New()
	publish(t, store, filepath.Join(dir, "child"), parentIn, childID,
		filepath.Join(dir, "parent"))

	child, err := OpenRemoteBuild(ctx, store, childID, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	defer child.Close()

	if _, err := child.Slice(ctx, 0, testBlock); err == nil {
		t.Error("a parent-served range resolved with no parent attached")
	}
}

// A build whose every block was elided has a zero-length data object, so any
// range against it is unsatisfiable. That means zeros, not failure -- treating
// it as an error is what kills a wake with an unrelated device timeout.
func TestRemoteBuildTreats416AsZeros(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	// An all-zero input: nothing is stored, so `data` is empty.
	in := writeBlocks(t, dir, 0, 0, 0)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	if len(store.objects[id.String()+"/data"]) != 0 {
		t.Fatalf("setup: data object is %d bytes, want 0",
			len(store.objects[id.String()+"/data"]))
	}

	b, err := OpenRemoteBuild(ctx, store, id, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	defer b.Close()

	got := readWhole(t, b)
	if !bytes.Equal(got, make([]byte, 3*testBlock)) {
		t.Error("an all-zero build did not read back as zeros")
	}
}

// A partially-elided build still has ranges past the end of its data object.
func TestRemoteBuildHandlesRangePastDataEnd(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 5, 0, 0)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	b, err := OpenRemoteBuild(ctx, store, id, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	defer b.Close()

	want, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhole(t, b); !bytes.Equal(got, want) {
		t.Errorf("differs at byte %d", firstDiff(got, want))
	}
}

// Contiguous missing blocks must become one request. Fetching per block turns
// a sequential read into thousands of round trips, which is the difference
// between a few seconds and an hour of paging.
func TestRemoteBuildCoalescesFetches(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2, 3, 4, 5, 6, 7, 8)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	b, err := OpenRemoteBuild(ctx, store, id, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	defer b.Close()

	readWhole(t, b)
	if store.ranges > 2 {
		t.Errorf("made %d range requests for eight contiguous blocks; they should coalesce",
			store.ranges)
	}
}

// Prefault pulls everything in one request, which is what stops a lazily
// faulted guest issuing one round trip per page.
func TestPrefaultFetchesEverythingOnce(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2, 3, 4)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	b, err := OpenRemoteBuild(ctx, store, id, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	defer b.Close()

	if err := b.Prefault(ctx); err != nil {
		t.Fatalf("Prefault: %v", err)
	}
	if store.ranges != 1 {
		t.Errorf("Prefault made %d requests, want 1", store.ranges)
	}

	before := store.ranges
	readWhole(t, b)
	if store.ranges != before {
		t.Errorf("reads after Prefault fetched again (%d -> %d)", before, store.ranges)
	}
}

// A build already complete on local disk must not be re-downloaded. Without
// this, a same-host wake refetches a file it already has and the guest sits
// waiting on pages it could have read immediately.
func TestRemoteBuildServesCompleteLocalDataWithoutFetching(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2, 3)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	cacheRoot := filepath.Join(dir, "cache")
	first, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	readWhole(t, first)
	first.Close()

	fetchesAfterFirst := store.ranges

	// A second open against the same cache root: the data is already there.
	second, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("second OpenRemoteBuild: %v", err)
	}
	defer second.Close()

	readWhole(t, second)
	if store.ranges != fetchesAfterFirst {
		t.Errorf("re-read fetched %d more ranges; complete local data should be used as is",
			store.ranges-fetchesAfterFirst)
	}
}

// OpenRemoteBuild writes the header next to the data so a later chunkify can
// open this build as a parent from disk alone, with no storage credentials.
func TestRemoteBuildCachesItsHeaderForChunkify(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	cacheRoot := filepath.Join(dir, "cache")
	b, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	readWhole(t, b) // materialize the data file
	b.Close()

	// The cache dir must now be openable as a plain local build.
	local, err := OpenLocalBuild(filepath.Join(cacheRoot, id.String()))
	if err != nil {
		t.Fatalf("the cached build is not usable as a chunkify parent: %v", err)
	}
	defer local.Close()

	want, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhole(t, local); !bytes.Equal(got, want) {
		t.Errorf("cached build differs at byte %d", firstDiff(got, want))
	}
}

// A cache file is TRUNCATED to its full packed size the moment it is created,
// so its length says nothing about how much was actually downloaded. A handler
// that exits partway leaves a full-length file full of holes -- and trusting
// the size alone makes the next open mark every block cached and serve those
// holes as zeros, with nothing erroring anywhere.
//
// This is reachable on an ordinary wake: the memory template is the parent of
// every machine's memory build.
func TestRemoteBuildDoesNotTrustAPartiallyDownloadedCache(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2, 3, 4)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	cacheRoot := filepath.Join(dir, "cache")

	// Open once and abandon it without fetching: this is what a killed handler
	// leaves behind -- the right length, none of the content.
	first, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	first.Close()

	info, err := os.Stat(filepath.Join(cacheRoot, id.String(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("setup: the abandoned cache file is empty, so it cannot model the hazard")
	}

	second, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("second OpenRemoteBuild: %v", err)
	}
	defer second.Close()

	want, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWhole(t, second); !bytes.Equal(got, want) {
		t.Errorf("a partially downloaded cache was served as complete; "+
			"differs at byte %d", firstDiff(got, want))
	}
}

// Once everything really is local, a re-open must not download it again.
func TestRemoteBuildTrustsACompletedCache(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	in := writeBlocks(t, dir, 1, 2, 3)
	id := uuid.New()
	publish(t, store, filepath.Join(dir, "build"), in, id, "")

	cacheRoot := filepath.Join(dir, "cache")
	first, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild: %v", err)
	}
	readWhole(t, first)
	first.Close()

	before := store.ranges
	second, err := OpenRemoteBuild(ctx, store, id, cacheRoot)
	if err != nil {
		t.Fatalf("second OpenRemoteBuild: %v", err)
	}
	defer second.Close()

	readWhole(t, second)
	if store.ranges != before {
		t.Errorf("a complete cache was re-downloaded (%d extra requests)",
			store.ranges-before)
	}
}

// A diff's parent-pointing ranges name a LOGICAL offset, not bytes, so any
// build attached as the parent resolves them to whatever it holds there. The
// header parses, every range resolves, and the guest comes back with another
// machine's memory. Which template a host has is local state -- a host with a
// cleared cache mints a fresh one with a fresh id -- so this is the only thing
// standing between a wake there and silent corruption.
func TestSetParentRejectsTheWrongTemplate(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	ctx := context.Background()

	parentIn := writeBlocks(t, dir, 1, 2, 3)
	parentID := uuid.New()
	publish(t, store, filepath.Join(dir, "parent"), parentIn, parentID, "")

	// A second template, of the same shape but different bytes: exactly what a
	// host that rebuilt its template has.
	otherIn := writeBlocks(t, dir, 7, 8, 9)
	otherID := uuid.New()
	publish(t, store, filepath.Join(dir, "other"), otherIn, otherID, "")

	childID := uuid.New()
	publish(t, store, filepath.Join(dir, "child"), parentIn, childID,
		filepath.Join(dir, "parent"))

	cacheRoot := filepath.Join(dir, "cache")
	child, err := OpenRemoteBuild(ctx, store, childID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(child): %v", err)
	}
	defer child.Close()

	wrong, err := OpenRemoteBuild(ctx, store, otherID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(other): %v", err)
	}
	defer wrong.Close()

	if err := child.SetParent(wrong); err == nil {
		t.Error("a build was accepted as the parent of a diff it never encoded")
	}

	right, err := OpenRemoteBuild(ctx, store, parentID, cacheRoot)
	if err != nil {
		t.Fatalf("OpenRemoteBuild(parent): %v", err)
	}
	defer right.Close()

	if err := child.SetParent(right); err != nil {
		t.Errorf("the correct parent was rejected: %v", err)
	}
}
