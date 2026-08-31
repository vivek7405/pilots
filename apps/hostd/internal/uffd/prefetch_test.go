package uffd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// Replaying a recorded fault order is what makes a warm wake fast: the pages
// are already installed when the guest resumes, so it never traps at all.
func TestReplayInstallsPagesBeforeTheGuestFaults(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages)
	path, want := memImage(t, testPages)

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
	prefault(context.Background(), h, src, list.Bytes(), &stats)

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
	fd, base, mem := newUserfaultfd(t, testPages)
	path, want := memImage(t, testPages)

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
	prefault(context.Background(), h, src, list.Bytes(), &stats)
	prefault(context.Background(), h, src, list.Bytes(), &stats)

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
