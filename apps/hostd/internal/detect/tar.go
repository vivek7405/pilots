package detect

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TarDir packs a directory into the tar a build takes, spooled to a file.
//
// It exists for the push path, which unpacks a GitHub tarball, plans it, and
// may write a generated Dockerfile into it before handing it to the builder.
// Repacking is the only way to get that file into the build context, since the
// archive GitHub served does not have it.
//
// A file rather than a bytes.Buffer, for the same reason the build route
// spools its upload: this runs on a host that is also running other tenants'
// microVMs, and the push path already holds one full copy of the repository
// while it works. A second one in RAM makes a large repository a
// memory-exhaustion lever that anyone with push access can pull.
//
// The caller closes what comes back. It is unlinked the moment it is created,
// so closing is all the cleanup there is and an early return cannot leak it.
//
// No modification times, so the same tree tars byte-identically twice. That is
// not cosmetic: a build cache keyed on the context would miss on every push if
// the timestamps moved.
func TarDir(dir string) (*os.File, error) {
	spool, err := os.CreateTemp(filepath.Dir(dir), "pilot-tar-*.tar")
	if err != nil {
		return nil, err
	}
	// Removed from the directory now, so the file lives exactly as long as the
	// handle does even if the caller returns on an error path below.
	_ = os.Remove(spool.Name())

	if err := writeTar(spool, dir); err != nil {
		spool.Close()
		return nil, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		spool.Close()
		return nil, err
	}
	return spool, nil
}

func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)

	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// The spool is created beside the directory rather than inside it, so
		// this is belt and braces; a tar that contained itself would grow for
		// as long as the disk allowed.
		if strings.HasPrefix(filepath.Base(rel), "pilot-tar-") {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.ModTime, hdr.AccessTime, hdr.ChangeTime = zeroTime, zeroTime, zeroTime
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// zeroTime is the timestamp every entry carries. The zero Time marshals as
// the tar epoch, which is what makes the archive reproducible.
var zeroTime = time.Time{}
