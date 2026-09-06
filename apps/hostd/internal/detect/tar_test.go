package detect

import (
	"os"
	"path/filepath"
	"testing"
)

// TarDir spools to a file rather than holding the archive in memory.
//
// The push path already carries one full copy of a repository while it works,
// and it runs on a host that is also running other tenants' microVMs. A second
// copy in RAM makes a large repository a memory-exhaustion lever that anyone
// with push access can pull, which is the same reason the build route spools
// its upload to disk.
//
// A file is also what proves it: a reader over a buffer and a reader over a
// file are indistinguishable to the caller, so the type is the assertion.
func TestTarDirSpoolsToAFileAndLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	mkdir(t, root, "repo")
	write(t, dir, "a.txt", "one")

	spool, err := TarDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()

	info, err := spool.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("the spool is empty")
	}

	// Unlinked at creation, so it is already gone from the directory and a
	// caller that returns early cannot leak it.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "repo" {
			t.Errorf("%s was left beside the context", e.Name())
		}
	}

	// And it reads back from the start: a spool handed to the builder mid-file
	// is an empty build context.
	dst := t.TempDir()
	if err := extractInto(spool, dst); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || string(body) != "one" {
		t.Errorf("a.txt = %q, %v", body, err)
	}
}
