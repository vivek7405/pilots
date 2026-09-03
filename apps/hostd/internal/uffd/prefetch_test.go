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

// A build mapping is block-aligned (4KiB); the guest faults at the page size.
// Handing UFFDIO_COPY a destination that is not page-aligned makes the kernel
// reject every copy, which is exactly what happened on the first hugepage
// host this ran on: 469 of 481 replayed pages failed.
func TestDiffEntriesAlignsToTheGuestPageSize(t *testing.T) {
	self := uuid.New()
	const huge = uint64(2) << 20
	src := buildWithMapping(self, []*block.BuildMap{
		// A 4KiB mapping sitting one block into a 2MiB page.
		{Offset: 4096, Length: 4096, BuildId: self},
	})

	got, _ := diffEntries(src, huge)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].off != 0 {
		t.Errorf("entry at offset %d, want it aligned down to 0", got[0].off)
	}
	if uint64(got[0].off)&(huge-1) != 0 {
		t.Errorf("offset %d is not %d-aligned, so the copy would be refused",
			got[0].off, huge)
	}
}

// Many small mappings inside one guest page must produce ONE entry, not one
// per mapping: replay installs a whole page at a time, so the rest would be
// redundant copies of the same page.
func TestDiffEntriesDeduplicatesWithinAPage(t *testing.T) {
	self := uuid.New()
	const huge = uint64(2) << 20
	var maps []*block.BuildMap
	for i := 0; i < 16; i++ {
		maps = append(maps, &block.BuildMap{
			Offset: uint64(i) * 4096, Length: 4096, BuildId: self,
		})
	}

	got, _ := diffEntries(buildWithMapping(self, maps), huge)
	if len(got) != 1 {
		t.Errorf("got %d entries for 16 mappings inside one 2MiB page, want 1",
			len(got))
	}
}

// A mapping that straddles a page boundary covers both pages.
func TestDiffEntriesCoversEveryPageAMappingTouches(t *testing.T) {
	self := uuid.New()
	const huge = uint64(2) << 20
	src := buildWithMapping(self, []*block.BuildMap{
		{Offset: huge - 4096, Length: 8192, BuildId: self},
	})

	got, _ := diffEntries(src, huge)
	if len(got) != 2 {
		t.Fatalf("got %d entries for a straddling mapping, want 2: %+v", len(got), got)
	}
	if got[0].off != 0 || uint64(got[1].off) != huge {
		t.Errorf("entries at %d and %d, want 0 and %d",
			got[0].off, got[1].off, huge)
	}
}

// shortSlicer hands back at most maxChunk bytes per call, which is what a real
// build does at a mapping boundary.
type shortSlicer struct {
	data     []byte
	maxChunk int
	calls    int
}

func (s *shortSlicer) Slice(_ context.Context, off, length int64) ([]byte, error) {
	s.calls++
	if off >= int64(len(s.data)) {
		return nil, block.ErrOutOfRange
	}
	end := off + length
	if end > int64(len(s.data)) {
		end = int64(len(s.data))
	}
	if int(end-off) > s.maxChunk {
		end = off + int64(s.maxChunk)
	}
	return s.data[off:end], nil
}

func (s *shortSlicer) BlockSize() int64               { return int64(s.maxChunk) }
func (s *shortSlicer) Size() int64                    { return int64(len(s.data)) }
func (s *shortSlicer) Prefault(context.Context) error { return nil }
func (s *shortSlicer) Close() error                   { return nil }

// THIS TEST STANDS BETWEEN THE CODEBASE AND A GUEST THAT NEVER COMES BACK.
//
// A Slicer stops at a mapping boundary, so filling one guest page can take
// several calls whenever the block size is smaller than the page size -- which
// is EVERY page once memory is backed by 2MiB pages and blocks are 4KiB. Taking
// the first short return as the end of the image installs a page that is part
// real data and part zeros. At 4KiB it cannot happen, because one block is one
// page; at 2MiB it corrupted almost every replayed page, and the restored guest
// resumed onto zeros and never ran. Found on a live host, not in a unit test.
func TestFillPageAssemblesAPageFromSeveralSlices(t *testing.T) {
	const pageSize = 64 * 1024
	want := make([]byte, pageSize)
	for i := range want {
		want[i] = byte(i%251 + 1) // no zeros, so a zero-fill is visible
	}
	src := &shortSlicer{data: want, maxChunk: 4096}

	buf := make([]byte, pageSize)
	if err := fillPage(context.Background(), src, 0, buf); err != nil {
		t.Fatalf("fillPage: %v", err)
	}
	if !bytes.Equal(buf, want) {
		for i := range buf {
			if buf[i] != want[i] {
				t.Fatalf("page differs at byte %d (got %#x want %#x); a single "+
					"Slice plus a zero-fill would produce exactly this",
					i, buf[i], want[i])
			}
		}
	}
	if src.calls < 2 {
		t.Errorf("filled the page in %d call(s); the fixture hands back at most "+
			"%d bytes, so this test is not exercising the loop", src.calls, src.maxChunk)
	}
}

// Past the end of the image the rest of the page is genuinely zeros, and that
// must still be true -- the loop must not turn a short image into an error.
func TestFillPageZeroFillsPastTheEndOfTheImage(t *testing.T) {
	const pageSize = 8192
	data := bytes.Repeat([]byte{0xAA}, 4096)
	src := &shortSlicer{data: data, maxChunk: 4096}

	buf := make([]byte, pageSize)
	for i := range buf {
		buf[i] = 0xFF // dirty, so a missing zero-fill is visible
	}
	if err := fillPage(context.Background(), src, 0, buf); err != nil {
		t.Fatalf("fillPage: %v", err)
	}
	if !bytes.Equal(buf[:4096], data) {
		t.Error("the part of the page inside the image was not filled")
	}
	if !bytes.Equal(buf[4096:], make([]byte, 4096)) {
		t.Error("the part past the end of the image was not zeroed")
	}
}
