package detect

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"time"
)

// TarDir packs a directory into the tar a build takes.
//
// It exists for the push path, which unpacks a GitHub tarball, plans it, and
// may write a generated Dockerfile into it before handing it to the builder.
// Repacking is the only way to get that file into the build context, since the
// archive GitHub served does not have it.
//
// No modification times, so the same tree tars byte-identically twice. That is
// not cosmetic: a build cache keyed on the context would miss on every push if
// the timestamps moved.
func TarDir(dir string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// zeroTime is the timestamp every entry carries. The zero Time marshals as
// the tar epoch, which is what makes the archive reproducible.
var zeroTime = time.Time{}
