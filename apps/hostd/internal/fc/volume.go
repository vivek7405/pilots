package fc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// BakedVolumePath is where a machine's persistent volume appears inside its
// jail, and therefore inside every snapshot it ever takes.
//
// Constant for the same reason BakedRootfsPath is: Firecracker's snapshot
// records the absolute path of every backing file and requires them to be
// "accessible at the same relative paths" on restore. A path naming the
// volume id, or the JuiceFS mount it really lives on, would pin the machine
// to the host that took the snapshot -- which is the one thing volumes exist
// to survive.
const BakedVolumePath = "/srv/pilots/volume.img"

// VolumeDriveID is the drive slot the volume occupies. The guest sees it as
// /dev/vdb, the rootfs being /dev/vda.
const VolumeDriveID = "volume"

// GuestVolumeDevice is where the volume drive appears inside the guest: the
// rootfs is vda and there is exactly one more drive. The guest agent has the
// same value as its own default; it is repeated rather than shared because the
// agent is a separate binary that runs inside the VM.
const GuestVolumeDevice = "/dev/vdb"

// CacheTypeWriteback is the ONLY correct cache type for a volume drive.
//
// Firecracker's default is "Unsafe", and Unsafe does not advertise the VirtIO
// flush feature at all. A guest that calls fsync on an Unsafe drive gets
// success back with the data sitting in the host's page cache, so every
// durability claim above it is a lie that no test notices until a host loses
// power. Writeback advertises flush and fsyncs the backing file when the guest
// asks for one, which is the entire reason a volume is a volume.
//
// See firecracker/docs/api_requests/block-caching.md.
const CacheTypeWriteback = "Writeback"

// stageVolume makes a volume's image reachable by the jailed Firecracker.
//
// A bind mount, not a copy and not a link. The image lives on a JuiceFS mount
// and must stay there -- that is what makes a write durable in object storage
// -- while the jailer chroots Firecracker somewhere on local disk, and a hard
// link cannot cross the two filesystems. Binding the file onto the constant
// path inside the chroot gives Firecracker a name it can open without moving
// a byte.
func stageVolume(chrootDir, image string, uid, gid int) error {
	if image == "" {
		return nil
	}
	if _, err := os.Stat(image); err != nil {
		return fmt.Errorf("fc: volume image %s: %w", image, err)
	}

	target := filepath.Join(chrootDir, BakedVolumePath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("fc: mkdir for volume image: %w", err)
	}

	// A leftover mount from a previous lifetime would silently shadow this
	// one: the bind would stack on top of it, and unmounting once at kill
	// would leave the older mount holding the old volume's file open forever.
	if err := unstageVolume(chrootDir); err != nil {
		return err
	}

	// The mountpoint has to exist and be a file, because the source is one.
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("fc: create volume mountpoint: %w", err)
	}
	f.Close()

	if err := unix.Mount(image, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("fc: bind %s onto %s: %w", image, target, err)
	}
	// Firecracker runs as the jail uid and writes to this file.
	if err := os.Chown(target, uid, gid); err != nil {
		return fmt.Errorf("fc: chown volume image: %w", err)
	}
	return nil
}

// unstageVolume releases the bind mount.
//
// This must run BEFORE the chroot directory is removed, and it is not
// housekeeping. RemoveAll cannot unlink a mountpoint, so a surviving bind
// fails the whole teardown -- and worse, it keeps the volume's file open, so
// `juicefs umount` refuses and the volume stays pinned to a host that no
// longer runs its machine. That is a volume that cannot be rescued anywhere.
//
// MNT_DETACH rather than a plain unmount: a Firecracker that has not finished
// dying still holds the file, and a lazy unmount detaches the tree now and
// releases it when the last reference goes.
func unstageVolume(chrootDir string) error {
	if chrootDir == "" {
		return nil
	}
	target := filepath.Join(chrootDir, BakedVolumePath)

	// Asked of the kernel's mount table rather than inferred from the error
	// umount(2) returns. Every machine without a volume comes through here, as
	// does every second kill, and both are the ordinary case -- while the
	// errno that says "not mounted" depends on who is asking (EINVAL as root,
	// EPERM otherwise), so swallowing it by value would also swallow a real
	// permission failure.
	mounted, err := isMountPoint(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	if err := unix.Unmount(target, unix.MNT_DETACH); err != nil &&
		!errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("fc: unmount volume image at %s: %w", target, err)
	}
	return nil
}

// isMountPoint reports whether path is a mount point right now.
//
// Read from /proc/self/mountinfo, whose fifth field is the mount point as an
// absolute path. Comparing device numbers with the parent directory would be
// the shorter version and is wrong here for the case that matters: it cannot
// tell a bind mount from a file that simply lives on another filesystem.
func isMountPoint(path string) (bool, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("fc: read mountinfo: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// Mountinfo octal-escapes the characters that would break the field
		// split; the paths here contain none of them, but unescaping keeps
		// the comparison honest for a machine id that ever does.
		if unescapeMountinfo(fields[4]) == path {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMountinfo(s string) string {
	for _, sub := range [][2]string{
		{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`},
	} {
		s = strings.ReplaceAll(s, sub[0], sub[1])
	}
	return s
}

// volumeDrive describes the volume as Firecracker's API wants it.
func volumeDrive() Drive {
	return Drive{
		DriveID:      VolumeDriveID,
		PathOnHost:   BakedVolumePath,
		IsRootDevice: false,
		IsReadOnly:   false,
		CacheType:    CacheTypeWriteback,
	}
}
