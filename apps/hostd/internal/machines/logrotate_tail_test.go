package machines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a writer appends DURING the rotation survives it.
//
// rotateLog copied the tail of the log to .1 and then truncated the original
// to zero. Firecracker keeps appending throughout that copy, and every byte it
// wrote after the read reached EOF was destroyed by a truncate that had no
// idea it was there. An 8 MiB copy is seconds; a guest panicking on its
// console during those seconds lost exactly the lines somebody was rotating
// the log to go and read, and rotateLog reported success.
//
// carryTail is driven directly here, because reproducing the race through
// rotateLog would mean writing to the file from another goroutine at exactly
// the right moment. Its contract is the fix: bytes past `from` are kept.
// Replacing it with os.Truncate(path, 0) reds this.
func TestBytesWrittenDuringARotationAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	// The first 100 bytes are what the copy read; the rest arrived while it
	// was running.
	copied := strings.Repeat("a", 100)
	during := "PANIC: the line somebody is rotating the log to read\n"
	if err := os.WriteFile(path, []byte(copied+during), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := carryTail(path, int64(len(copied))); err != nil {
		t.Fatalf("carryTail: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != during {
		t.Errorf("the log now holds %q, want the bytes that arrived during the copy", got)
	}
}

// The ordinary case: nothing arrived, and the log is emptied as before.
func TestARotationWithNoLateWritesEmptiesTheLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	body := strings.Repeat("a", 100)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := carryTail(path, int64(len(body))); err != nil {
		t.Fatalf("carryTail: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("the log is %d bytes, want empty", info.Size())
	}
}

// A writer that produced more than logKeep during one copy does not get read
// wholly into memory, and the LAST logKeep survives -- the same end-over-
// beginning rule the rotation itself follows.
func TestAnEnormousLateWriteIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	head := strings.Repeat("a", 10)
	late := strings.Repeat("b", int(logKeep)+4096) + "END"
	if err := os.WriteFile(path, []byte(head+late), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := carryTail(path, int64(len(head))); err != nil {
		t.Fatalf("carryTail: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) > logKeep {
		t.Errorf("kept %d bytes, want at most logKeep (%d)", len(got), logKeep)
	}
	if !strings.HasSuffix(string(got), "END") {
		t.Error("the END of the late write was dropped; a rotation keeps the end")
	}
}

// A whole rotation keeps what arrived, end to end, rather than only the helper
// doing so.
func TestRotateLogPreservesTheFileItTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	body := strings.Repeat("x", logRotateAt+1)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	rotated, err := rotateLog(path)
	if err != nil {
		t.Fatalf("rotateLog: %v", err)
	}
	if !rotated {
		t.Fatal("a log over the limit was not rotated")
	}
	kept, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(kept)) != logKeep {
		t.Errorf(".1 holds %d bytes, want logKeep (%d)", len(kept), logKeep)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("the live log is %d bytes after a rotation with no late writes, "+
			"want empty", info.Size())
	}
}
