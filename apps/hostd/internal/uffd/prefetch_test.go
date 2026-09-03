package uffd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// Replaying a recorded fault order is what makes a warm wake fast: the pages
// are already installed when the guest resumes, so it never traps at all.
func TestReplayInstallsPagesBeforeTheGuestFaults(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages, testPageSize)
	path, want := memImage(t, testPages, testPageSize)

	src, err := block.OpenFileSlicer(path, int64(testPageSize))
	if err != nil {
		t.Fatalf("OpenFileSlicer: %v", err)
	}
	defer src.Close()

	h := &handshake{
		uffd: fd, pageSize: testPageSize,
		regions: []Region{{
			BaseHostVirtAddr: base,
			Size:             uint64(testPages) * testPageSize,
			PageSize:         testPageSize,
		}},
	}

	var list bytes.Buffer
	for i := 0; i < testPages; i++ {
		fmt.Fprintf(&list, "%d %d\n", int64(i)*int64(testPageSize), testPageSize)
	}

	var stats Stats
	prefault(context.Background(), h, src, list.Bytes(), nil, &stats)

	// No serve loop is running. If replay had not installed these pages, the
	// read below would trap into a handler that is not there and block
	// forever -- so reaching the comparison at all is part of the assertion.
	got := make([]byte, len(mem))
	copy(got, mem)
	if !bytes.Equal(got, want) {
		t.Errorf("replayed memory differs at byte %d", firstDiff(got, want))
	}

	// Replay is not a fault: counting it would make the recorded list grow
	// with pages the guest never actually asked for, and every subsequent
	// wake would replay more than it needs.
	if n := stats.Faults.Load(); n != 0 {
		t.Errorf("replay counted %d faults; only on-demand faults belong in that number", n)
	}
}

// Replaying a list twice must be harmless. A page that is already installed
// comes back EEXIST, which is success -- the alternative is a wake that fails
// because the prefetch raced the serve loop, which is exactly what they are
// designed to do.
func TestReplayIsIdempotent(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages, testPageSize)
	path, want := memImage(t, testPages, testPageSize)

	src, err := block.OpenFileSlicer(path, int64(testPageSize))
	if err != nil {
		t.Fatalf("OpenFileSlicer: %v", err)
	}
	defer src.Close()

	h := &handshake{
		uffd: fd, pageSize: testPageSize,
		regions: []Region{{
			BaseHostVirtAddr: base,
			Size:             uint64(testPages) * testPageSize,
			PageSize:         testPageSize,
		}},
	}

	var list bytes.Buffer
	for i := 0; i < testPages; i++ {
		fmt.Fprintf(&list, "%d %d\n", int64(i)*int64(testPageSize), testPageSize)
	}

	var stats Stats
	prefault(context.Background(), h, src, list.Bytes(), nil, &stats)
	prefault(context.Background(), h, src, list.Bytes(), nil, &stats)

	if stats.CopyEEXIST.Load() == 0 {
		t.Error("the second replay did not hit any already-installed page; " +
			"this test can no longer observe the idempotence it checks")
	}
	if n := stats.CopyFailed.Load(); n != 0 {
		t.Errorf("%d copies failed on replay", n)
	}

	got := make([]byte, len(mem))
	copy(got, mem)
	if !bytes.Equal(got, want) {
		t.Errorf("memory differs after a double replay at byte %d", firstDiff(got, want))
	}
}

// The replay list and the capture file are normally the same path -- that is
// the point, each wake refines the last one's ordering. Creating the recorder
// truncates it, so reading second means replaying an empty list and silently
// losing the optimisation on every wake after the first.
func TestPrefetchIsReadBeforeTheRecorderTruncatesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefetch.txt")
	if err := os.WriteFile(path, []byte("0 4096\n4096 4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	contents := readPrefetch(path)
	if len(parsePrefetch(contents)) != 2 {
		t.Fatalf("read %d entries before recording, want 2", len(parsePrefetch(contents)))
	}

	rec, err := newRecorder(path)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	defer rec.Close()

	// The file is empty now -- which is exactly why the read has to come
	// first. The contents already in hand are unaffected.
	if got := readPrefetch(path); len(parsePrefetch(got)) != 0 {
		t.Error("newRecorder did not truncate; this test no longer models the hazard")
	}
	if len(parsePrefetch(contents)) != 2 {
		t.Error("the previously-read list was lost when the recorder opened the file")
	}
}

// A machine's first wake has no list. That is ordinary, not an error, and it
// must not stop the handler from starting.
func TestReadPrefetchToleratesAMissingFile(t *testing.T) {
	if got := readPrefetch(filepath.Join(t.TempDir(), "absent.txt")); got != nil {
		t.Errorf("a missing prefetch file returned %q", got)
	}
	if got := readPrefetch(""); got != nil {
		t.Errorf("an unset prefetch path returned %q", got)
	}
}

// A truncated final line -- a handler that was SIGKILLed mid-write -- must
// cost that one entry, not the whole list.
func TestParsePrefetchSkipsCorruptLines(t *testing.T) {
	entries := parsePrefetch([]byte("0 4096\ngarbage\n8192 4096\n4096"))
	if len(entries) != 2 {
		t.Fatalf("parsed %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].off != 0 || entries[1].off != 8192 {
		t.Errorf("parsed offsets %d and %d, want 0 and 8192", entries[0].off, entries[1].off)
	}
}

// fakeHeadered is a Slicer that reports a build layout without any storage
// behind it, which is all diffEntries reads.
type fakeHeadered struct {
	block.Slicer
	hdr *block.Header
}

func (f *fakeHeadered) Header() *block.Header { return f.hdr }

func buildWithMapping(self uuid.UUID, maps []*block.BuildMap) *fakeHeadered {
	return &fakeHeadered{hdr: &block.Header{
		Metadata: &block.Metadata{BuildId: self},
		Mapping:  maps,
	}}
}

// The ranges a build stores ITSELF are exactly what the machine changed since
// the template. Ranges pointing at the parent are unchanged, so fetching them
// ahead of demand spends bandwidth to install pages the guest may never touch.
func TestDiffEntriesTakesOnlyTheBuildsOwnRanges(t *testing.T) {
	self, parent := uuid.New(), uuid.New()
	src := buildWithMapping(self, []*block.BuildMap{
		{Offset: 0, Length: 4096, BuildId: self},
		{Offset: 4096, Length: 4096, BuildId: parent},
		{Offset: 8192, Length: 4096, BuildId: self},
	})

	got, dropped := diffEntries(src, 4096)
	if dropped != 0 {
		t.Errorf("dropped %d under the cap, want 0", dropped)
	}
	want := []int64{0, 8192}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, off := range want {
		if got[i].off != off {
			t.Errorf("entry %d at offset %d, want %d", i, got[i].off, off)
		}
	}
}

// A mapping can span many pages, and replay installs one page per entry.
func TestDiffEntriesSplitsAMappingIntoPages(t *testing.T) {
	self := uuid.New()
	src := buildWithMapping(self, []*block.BuildMap{
		{Offset: 0, Length: 4 * 4096, BuildId: self},
	})

	got, _ := diffEntries(src, 4096)
	if len(got) != 4 {
		t.Fatalf("got %d entries for a 4-page mapping, want 4", len(got))
	}
	for i, e := range got {
		if e.off != int64(i*4096) || e.length != 4096 {
			t.Errorf("entry %d = {off:%d len:%d}, want {off:%d len:4096}",
				i, e.off, e.length, i*4096)
		}
	}
}

// Uncapped, a machine that rewrote most of its memory turns its wake into a
// full image read: the fault storm the bulk fetch exists to avoid, moved one
// layer up.
func TestDiffEntriesIsCapped(t *testing.T) {
	self := uuid.New()
	const mappingLen = 8 << 20
	var maps []*block.BuildMap
	for i := 0; i < 32; i++ { // 256MiB of self-pointing ranges
		maps = append(maps, &block.BuildMap{
			Offset: uint64(i * mappingLen), Length: mappingLen, BuildId: self,
		})
	}

	got, dropped := diffEntries(buildWithMapping(self, maps), 2<<20)
	if dropped == 0 {
		t.Error("nothing was dropped for a 256MiB diff, so the cap did not apply")
	}

	var span int64
	for _, e := range got {
		span += e.length
	}
	if span > prefetchDiffCap {
		t.Errorf("replay span is %d bytes, over the %d cap", span, prefetchDiffCap)
	}
}

// A machine restored straight from the template has no diff of its own, and a
// source that cannot describe its layout at all must simply contribute
// nothing rather than fail the wake.
func TestDiffEntriesIsEmptyWithoutAHeader(t *testing.T) {
	if got, _ := diffEntries(nil, 4096); got != nil {
		t.Errorf("a nil source produced %d entries", len(got))
	}
	if got, _ := diffEntries(&fakeHeadered{}, 4096); got != nil {
		t.Errorf("a source with no header produced %d entries", len(got))
	}
}

// The recorded order is a sequence that matches the guest's real access
// order; the diff ranges are an unordered set. Putting the set first would
// delay the pages the guest is about to ask for.
func TestPrefaultReplaysTheRecordedOrderBeforeTheDiff(t *testing.T) {
	entries := append(parsePrefetch([]byte("100 4096\n")), entry{off: 0, length: 4096})
	if entries[0].off != 100 {
		t.Errorf("first replayed entry is at %d, want the recorded 100", entries[0].off)
	}
	if entries[1].off != 0 {
		t.Errorf("second replayed entry is at %d, want the diff's 0", entries[1].off)
	}
}
