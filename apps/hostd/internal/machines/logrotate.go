package machines

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// Keeping console logs from growing without bound.
//
// # Why copytruncate and not a rename
//
// Firecracker holds the log file open for the life of the machine and writes
// through that descriptor. Renaming the file would leave it writing to the
// renamed inode, so the "new" log would stay empty for ever and the rotated one
// would keep growing: the exact failure a rotation exists to prevent, arrived
// at by rotating.
//
// Copying the tail into a second file and TRUNCATING the original keeps the
// inode, so the open descriptor keeps working. That only holds because the file
// is opened O_APPEND: a plain O_WRONLY descriptor keeps its own offset, so
// after a truncate it would write at the offset it had reached and leave a
// multi-megabyte hole of zero bytes in front of every new line.
//
// # Why 8 MiB, and why on NVMe
//
// Two files of 8 MiB is 16 MiB per machine, which a host can hold for a
// thousand machines without thinking about it, and which is far more console
// output than anybody reads. It lives on the machine's state directory and is
// NOT machine state: wipe a host and the logs are gone while the machine
// restores from object storage exactly as before. Shipping them anywhere would
// be a fourth process and a tier, which is the thing this architecture is
// arranged to avoid.

const (
	// logRotateAt is when a log is rotated: the size the live file may reach.
	logRotateAt = 8 << 20
	// logKeep is how much of it survives into the rotated copy. The whole
	// ceiling, so a rotation never throws away more than it has to.
	logKeep = 8 << 20
)

// rotateLog copies the tail of a machine's console log aside and truncates it.
//
// Returns true when it rotated, so a caller can log that once rather than
// deciding for itself whether anything happened.
func rotateLog(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= logRotateAt {
		return false, nil
	}

	src, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer src.Close()

	// The LAST logKeep bytes, because the end of a log is the part somebody is
	// reading. A rotation that kept the beginning would preserve a boot message
	// and discard the crash.
	at := info.Size() - logKeep
	if at < 0 {
		at = 0
	}
	if _, err := src.Seek(at, io.SeekStart); err != nil {
		return false, err
	}

	// Written to a temporary name and renamed, so a reader of the .1 file never
	// sees a half-copied one. The rename is atomic within the directory.
	tmp := path + ".rotating"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, path+".1"); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}

	// Truncate rather than replace: the writer holds this inode open, and an
	// O_APPEND descriptor resumes at the new end of the file by definition.
	if err := os.Truncate(path, 0); err != nil {
		return false, err
	}
	return true, nil
}

// rotateLogs walks this host's machines once and rotates what is too big.
//
// Rides the idle monitor's existing walk rather than adding a loop of its own:
// that loop already lists exactly the rows this needs, on a cadence that is
// already right for a file that takes hours to fill.
func (m *Manager) rotateLogs(ids []string) {
	for _, id := range ids {
		path := filepath.Join(m.stateDir(id), "lifecycle.log")
		rotated, err := rotateLog(path)
		if err != nil {
			slog.Warn("could not rotate a console log", "machine", id, "err", err)
			continue
		}
		if rotated {
			slog.Info("rotated a console log", "machine", id)
		}
	}
}
