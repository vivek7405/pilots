package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The guest half of a volume.
//
// The volume arrives as a second virtio-blk device, /dev/vdb, holding an ext4
// filesystem the host already made. hostd calls this after every resume, in
// the same place it pokes the clock, because the mount is guest state and a
// restore brings back a guest that was captured on some other host.
//
// The mount path is delivered rather than baked. It is per machine, and
// anything per machine that ended up inside the golden template would be
// wrong for every machine created from it.

// VolumeDevice is where Firecracker's second drive appears. First drive is
// vda, the rootfs; there is exactly one more.
const VolumeDevice = "/dev/vdb"

type volumeRequest struct {
	Device    string `json:"device,omitempty"`
	MountPath string `json:"mount_path"`
}

// handleVolume mounts the machine's volume, formatting it only if it has never
// been formatted.
//
// Idempotent: called again on a machine that already has it mounted, it does
// nothing and reports success. hostd calls it on every resume without knowing
// whether this particular resume needed it.
func handleVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}
	if req.Device == "" {
		req.Device = VolumeDevice
	}
	if req.MountPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mount_path is required"})
		return
	}
	if !filepath.IsAbs(req.MountPath) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mount_path must be absolute"})
		return
	}

	if err := mountVolume(req.Device, req.MountPath); err != nil {
		log.Printf("guest-agent: mount volume: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mount_path": req.MountPath})
}

func mountVolume(device, mountPath string) error {
	if _, err := os.Stat(device); err != nil {
		return fmt.Errorf("no volume device at %s: %w", device, err)
	}
	if mounted, err := deviceMountedAt(mountPath); err != nil {
		return err
	} else if mounted {
		return nil
	}

	// Formatting is the one irreversible thing this function can do, so it
	// happens only when the device demonstrably has no filesystem on it. The
	// host formats the image at create time; this covers a volume whose create
	// died between making the image and formatting it, and nothing else.
	formatted, err := hasFilesystem(device)
	if err != nil {
		return err
	}
	if !formatted {
		log.Printf("guest-agent: %s has no filesystem; creating ext4", device)
		if out, err := combinedOutputTracked(exec.Command("mke2fs", "-q", "-t", "ext4", device)); err != nil {
			return fmt.Errorf("mke2fs %s: %w: %s", device, err, strings.TrimSpace(string(out)))
		}
	}

	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", mountPath, err)
	}
	if err := unix.Mount(device, mountPath, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", device, mountPath, err)
	}
	return nil
}

// hasFilesystem reports whether a device already carries an ext4 superblock.
//
// Read directly rather than shelled out to blkid, which the golden rootfs does
// not carry. The ext2/3/4 superblock lives 1024 bytes in and its magic is the
// two bytes 0x53 0xEF at offset 56 within it -- the same check e2fsprogs makes
// before it will touch a device.
//
// This is the difference between a rescued volume and an erased one, so it
// fails closed: an unreadable device is reported as formatted, because
// refusing to mount is recoverable and reformatting is not.
func hasFilesystem(device string) (bool, error) {
	f, err := os.Open(device)
	if err != nil {
		return true, fmt.Errorf("open %s: %w", device, err)
	}
	defer f.Close()

	buf := make([]byte, 2)
	if _, err := f.ReadAt(buf, 1024+56); err != nil {
		return true, fmt.Errorf("read the superblock of %s: %w", device, err)
	}
	return buf[0] == 0x53 && buf[1] == 0xEF, nil
}

// deviceMountedAt reports whether anything is mounted at path.
func deviceMountedAt(path string) (bool, error) {
	raw, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false, fmt.Errorf("read mounts: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && unescapeMounts(fields[1]) == path {
			return true, nil
		}
	}
	return false, nil
}

func unescapeMounts(s string) string {
	for _, sub := range [][2]string{
		{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`},
	} {
		s = strings.ReplaceAll(s, sub[0], sub[1])
	}
	return s
}
