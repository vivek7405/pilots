package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The check that decides whether a volume gets reformatted. Getting it wrong
// in one direction refuses to mount; getting it wrong in the other erases a
// user's data, so the two are not symmetric and this test cares about both.
func TestHasFilesystem(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs is not installed")
	}
	dir := t.TempDir()

	blank := filepath.Join(dir, "blank.img")
	if err := os.WriteFile(blank, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := hasFilesystem(blank); err != nil || got {
		t.Fatalf("hasFilesystem(blank) = %v, %v; want false", got, err)
	}

	formatted := filepath.Join(dir, "ext4.img")
	if err := os.WriteFile(formatted, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", formatted).CombinedOutput(); err != nil {
		t.Fatalf("mke2fs: %v: %s", err, out)
	}
	if got, err := hasFilesystem(formatted); err != nil || !got {
		t.Fatalf("hasFilesystem(ext4) = %v, %v; want true", got, err)
	}
}

// Fails closed. A device that cannot be read is reported as already carrying a
// filesystem, because refusing to mount is recoverable and reformatting
// someone's volume is not.
func TestHasFilesystemFailsClosed(t *testing.T) {
	got, err := hasFilesystem(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("reading a device that is not there returned no error")
	}
	if !got {
		t.Fatal("an unreadable device was reported as unformatted, which would " +
			"reformat a volume whose device was merely slow to appear")
	}

	// A file too short to hold a superblock is the same case: not proof that
	// there is no filesystem.
	short := filepath.Join(t.TempDir(), "short.img")
	if err := os.WriteFile(short, make([]byte, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := hasFilesystem(short); err == nil || !got {
		t.Fatalf("hasFilesystem(short) = %v, %v; want true with an error", got, err)
	}
}

func TestDeviceMountedAt(t *testing.T) {
	// /proc itself is always mounted wherever this runs, and nothing is
	// mounted inside a fresh temp dir.
	if got, err := deviceMountedAt("/proc"); err != nil || !got {
		t.Fatalf("deviceMountedAt(/proc) = %v, %v; want true", got, err)
	}
	if got, err := deviceMountedAt(t.TempDir()); err != nil || got {
		t.Fatalf("deviceMountedAt(tempdir) = %v, %v; want false", got, err)
	}
}
