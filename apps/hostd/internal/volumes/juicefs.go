package volumes

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// metaURL is the JuiceFS META-URL for a volume.
//
// sqlite3://, not redis:// or postgres://. The documented caveat on the SQLite
// engine -- "usually only the host where the database is located can access
// it ... more suitable for standalone use" -- describes our model exactly,
// because a volume is mounted on one host at a time anyway. Choosing a
// networked engine instead would reintroduce the central metadata service the
// architecture exists to avoid.
func (m *Manager) metaURL(id string) string { return "sqlite3://" + m.MetaPath(id) }

// formatArgs builds `juicefs format`.
//
//   - --block-size is in KiB and matches the 4KiB blocks everything else in
//     this engine addresses in.
//   - --compress none keeps ranges byte-addressable: a compressed object
//     cannot serve a partial read of the image without inflating the whole
//     chunk, and the guest reads this image in scattered 4KiB pieces forever.
//   - --trash-days 0 because the image is one file that is never deleted;
//     a trash retention would keep a copy of every overwritten chunk and
//     silently multiply what the volume costs to store.
func (m *Manager) formatArgs(id string) []string {
	return []string{
		"format",
		"--storage", "s3",
		"--bucket", m.bucketURL(),
		"--access-key", m.cfg.AccessKey,
		"--secret-key", m.cfg.SecretKey,
		"--block-size", "4096",
		"--compress", "none",
		"--trash-days", "0",
		m.metaURL(id),
		id,
	}
}

// mountArgs builds `juicefs mount`.
//
// What is NOT here is the point: --writeback is deliberately absent, and must
// stay absent. It makes JuiceFS acknowledge a write once it is in the local
// cache and upload it asynchronously, which is the exact opposite of the
// per-write durability a volume exists to provide. It is also the flag that
// makes the "volume survives a host kill" test pass on a laptop and fail for
// real, because the window it opens is only visible when the host dies inside
// it. The predecessor used it correctly for snapshots, which are whole-file
// and write-once; a volume is neither.
func (m *Manager) mountArgs(id string) []string {
	return []string{
		"mount", "-d",
		"--cache-dir", m.CacheDir(id),
		"--cache-size", strconv.Itoa(m.cfg.CacheSizeMiB),
		"--free-space-ratio", "0.15",
		"--max-uploads", "20",
		// A periodic metadata dump into object storage, so a volume is
		// recoverable even if the SQLite file and its Litestream replica are
		// both lost.
		"--backup-meta", "3600",
		m.metaURL(id),
		m.MountPoint(id),
	}
}

// format creates the filesystem. Runs once, on the host that creates the
// volume, and never again -- formatting a volume that already has data throws
// the data away.
func (m *Manager) format(ctx context.Context, id string) error {
	if _, err := m.run(ctx, m.cfg.JuiceFSBin, m.formatArgs(id)...); err != nil {
		return fmt.Errorf("volumes: format %s: %w", id, err)
	}
	return nil
}

// mount attaches the volume's filesystem on this host.
func (m *Manager) mount(ctx context.Context, id string) error {
	if mounted, err := m.Mounted(id); err != nil {
		return err
	} else if mounted {
		return nil
	}
	if _, err := m.run(ctx, m.cfg.JuiceFSBin, m.mountArgs(id)...); err != nil {
		return fmt.Errorf("volumes: mount %s: %w", id, err)
	}
	return nil
}

// Unmount detaches the volume's filesystem.
//
// The ordering around this is what makes a volume movable: the guest has to
// have stopped writing, the machine has to be gone, and only then can the
// filesystem be released and its metadata allowed to settle in object storage.
// Unmounting under a live guest is a lost write, not an error.
func (m *Manager) Unmount(ctx context.Context, id string) error {
	if mounted, err := m.Mounted(id); err != nil {
		return err
	} else if !mounted {
		return nil
	}
	if _, err := m.run(ctx, m.cfg.JuiceFSBin, "umount", m.MountPoint(id)); err != nil {
		return fmt.Errorf("volumes: unmount %s: %w", id, err)
	}
	return nil
}

// Mounted reports whether a volume's filesystem is attached here.
func (m *Manager) Mounted(id string) (bool, error) {
	raw, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false, fmt.Errorf("volumes: read mounts: %w", err)
	}
	return hasMountPoint(string(raw), m.MountPoint(id)), nil
}

// hasMountPoint reports whether any line of a /proc mounts table mounts
// something at target.
//
// The kernel escapes the characters that would break the field split, so the
// second field is unescaped before it is compared. Volume ids contain none of
// them; the unescaping is here so that stays true of whatever mounts at these
// paths later rather than by luck.
func hasMountPoint(table, target string) bool {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if unescapeMounts(fields[1]) == target {
			return true
		}
	}
	return false
}

func unescapeMounts(s string) string {
	for _, sub := range [][2]string{
		{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`},
	} {
		s = strings.ReplaceAll(s, sub[0], sub[1])
	}
	return s
}
