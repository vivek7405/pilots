package fc

import (
	"os"
	"os/exec"
	"path/filepath"
)

// SupportsReflink reports whether dir sits on a filesystem that can share
// extents between files.
//
// This is not a filesystem trivium: every headline latency in the instant
// engine assumes that duplicating a multi-gigabyte image is a metadata
// operation. Where reflinks are unavailable, `cp --reflink=auto` quietly does
// the copy for real. Create then grows by however long it takes to duplicate
// the whole template -- measured at 2.2s for a 2GiB rootfs on ext4, against a
// 1.5s budget for the entire operation -- and the checkpoint pause stops being
// independent of machine size, which is the property that makes checkpoints
// usable at all.
//
// Nothing errors when that happens. The platform is simply several times
// slower than it claims to be, everywhere, forever. So the support is probed
// explicitly at startup and reported, rather than assumed and silently lost.
func SupportsReflink(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	src, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return false
	}
	srcPath := src.Name()
	dstPath := filepath.Join(dir, filepath.Base(srcPath)+".clone")
	defer func() {
		_ = os.Remove(srcPath)
		_ = os.Remove(dstPath)
	}()

	// A file with a real extent to share. Cloning an empty file would prove
	// nothing about whether extents can actually be shared.
	if _, err := src.Write(make([]byte, 4096)); err != nil {
		_ = src.Close()
		return false
	}
	if err := src.Close(); err != nil {
		return false
	}

	// --reflink=always is the point: it fails rather than falling back, which
	// is exactly the fallback the engine cannot see when it uses "auto".
	return exec.Command("cp", "--reflink=always", srcPath, dstPath).Run() == nil
}
