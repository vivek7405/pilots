package machines

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func bigLog(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	body := make([]byte, size)
	for i := range body {
		body[i] = 'a'
		if i%80 == 79 {
			body[i] = '\n'
		}
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestASmallLogIsLeftAlone(t *testing.T) {
	path := bigLog(t, 1024)
	rotated, err := rotateLog(path)
	if err != nil {
		t.Fatalf("rotateLog: %v", err)
	}
	if rotated {
		t.Error("a 1 KiB log was rotated")
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Error("a rotated file was written for a log that did not need it")
	}
}

func TestABigLogIsRotatedAndTheOriginalEmptied(t *testing.T) {
	path := bigLog(t, logRotateAt+4096)
	rotated, err := rotateLog(path)
	if err != nil {
		t.Fatalf("rotateLog: %v", err)
	}
	if !rotated {
		t.Fatal("a log past the ceiling was not rotated")
	}

	live, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat live: %v", err)
	}
	if live.Size() != 0 {
		t.Errorf("the live log is %d bytes after a rotation, want 0", live.Size())
	}
	kept, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("stat rotated: %v", err)
	}
	if kept.Size() != logKeep {
		t.Errorf("kept %d bytes, want %d", kept.Size(), logKeep)
	}
	// Nothing half-copied left behind: a reader that found the temporary name
	// would read a file that is still being written.
	if _, err := os.Stat(path + ".rotating"); !os.IsNotExist(err) {
		t.Error("the temporary copy was left behind")
	}
}

// The END of the log is what somebody is reading. A rotation that kept the
// beginning would preserve a boot message and discard the crash.
func TestARotationKeepsTheEndAndNotTheBeginning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.log")
	body := append(append([]byte("FIRST-LINE\n"), make([]byte, logRotateAt)...), []byte("\nLAST-LINE\n")...)
	for i := 11; i < len(body)-11; i++ {
		body[i] = 'x'
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := rotateLog(path); err != nil {
		t.Fatalf("rotateLog: %v", err)
	}
	kept, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Contains(kept, []byte("LAST-LINE")) {
		t.Error("the end of the log was discarded")
	}
	if bytes.Contains(kept, []byte("FIRST-LINE")) {
		t.Error("the rotation kept more than the ceiling")
	}
}

// The whole reason copytruncate is safe, and the counterexample that shows why
// the flag matters.
//
// An O_APPEND writer resumes at the new end of a truncated file. A plain
// O_WRONLY writer keeps the offset it had reached and writes THERE, leaving a
// multi-megabyte hole of zero bytes in front of every later line. Both halves
// are asserted, because the first is only meaningful next to the second.
func TestAnAppendingWriterSurvivesATruncateAndAPlainOneDoesNot(t *testing.T) {
	dir := t.TempDir()

	appendPath := filepath.Join(dir, "append.log")
	appending, err := os.OpenFile(appendPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer appending.Close()
	if _, err := appending.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Truncate(appendPath, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := appending.Write([]byte("after\n")); err != nil {
		t.Fatalf("write after truncate: %v", err)
	}
	info, err := os.Stat(appendPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len("after\n")) {
		t.Errorf("the appending writer left a %d byte file, want %d: it did not "+
			"resume at the new end", info.Size(), len("after\n"))
	}

	// The counterexample. Same sequence, no O_APPEND.
	plainPath := filepath.Join(dir, "plain.log")
	plain, err := os.OpenFile(plainPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer plain.Close()
	if _, err := plain.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Truncate(plainPath, 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := plain.Write([]byte("after\n")); err != nil {
		t.Fatalf("write after truncate: %v", err)
	}
	info, err = os.Stat(plainPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == int64(len("after\n")) {
		t.Error("a plain O_WRONLY writer resumed at the start, so this platform " +
			"would not show the hole and the O_APPEND flag proves nothing here")
	}
}
