package fc

import (
	"os"
	"path/filepath"
	"testing"
)

// The real thing: bind a file onto the baked path, prove it is reachable and
// is the same bytes, and prove the teardown releases it. The release is the
// half that matters -- a bind left behind pins the volume's file open, and
// juicefs then refuses to unmount it on a host that no longer runs the
// machine.
func TestStageAndUnstageVolumeBindMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: bind mounts")
	}

	chroot := t.TempDir()
	image := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(image, []byte("volume-contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := stageVolume(chroot, image, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatalf("stageVolume: %v", err)
	}
	t.Cleanup(func() { _ = unstageVolume(chroot) })

	target := filepath.Join(chroot, BakedVolumePath)
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read through the bind: %v", err)
	}
	if string(got) != "volume-contents" {
		t.Fatalf("the jail sees %q, not the volume image", got)
	}
	if mounted, err := isMountPoint(target); err != nil || !mounted {
		t.Fatalf("isMountPoint(%s) = %v, %v; want true", target, mounted, err)
	}

	// Staging twice must not stack: the second bind would hide the first, and
	// one unmount at kill would leave the hidden one holding the file.
	if err := stageVolume(chroot, image, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatalf("re-staging: %v", err)
	}

	if err := unstageVolume(chroot); err != nil {
		t.Fatalf("unstageVolume: %v", err)
	}
	if mounted, err := isMountPoint(target); err != nil || mounted {
		t.Fatalf("still mounted after unstaging (err %v); the chroot cannot be removed "+
			"and the volume cannot be unmounted anywhere", err)
	}
	// And the chroot is removable, which is what Kill does next.
	if err := os.RemoveAll(chroot); err != nil {
		t.Fatalf("remove the chroot after unstaging: %v", err)
	}
}
