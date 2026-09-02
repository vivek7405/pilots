package fc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gate line this file exists for. Firecracker's default cache type does
// not advertise the VirtIO flush feature, so a guest fsync to a volume drive
// left at the default returns success with the data only in the host's page
// cache. The value is set in exactly one place; assert that it is still there
// and still spelled the way Firecracker's API expects.
func TestVolumeDriveRequestsWriteback(t *testing.T) {
	raw, err := json.Marshal(volumeDrive())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got["cache_type"] != "Writeback" {
		t.Fatalf("volume drive cache_type is %v, not \"Writeback\": a guest fsync "+
			"to this drive would be a no-op", got["cache_type"])
	}
	if got["path_on_host"] != BakedVolumePath {
		t.Errorf("path_on_host is %v, want the constant %s -- a host-specific "+
			"path here pins the machine to the host that snapshotted it",
			got["path_on_host"], BakedVolumePath)
	}
	if got["is_root_device"] != false {
		t.Errorf("the volume claims to be the root device")
	}
	if got["is_read_only"] != false {
		t.Errorf("the volume is read only")
	}
}

// The rootfs drive deliberately says nothing about caching, so the field must
// be omitted rather than sent empty -- Firecracker rejects an empty string.
func TestDriveOmitsCacheTypeWhenUnset(t *testing.T) {
	raw, err := json.Marshal(Drive{DriveID: "rootfs", PathOnHost: BakedRootfsPath, IsRootDevice: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "cache_type") {
		t.Fatalf("an unset cache type was still sent: %s", raw)
	}
}

// Every machine without a volume goes through these, so they have to be inert
// rather than merely tolerated.
func TestStageVolumeIsANoOpWithoutAnImage(t *testing.T) {
	if err := stageVolume(t.TempDir(), "", 0, 0); err != nil {
		t.Fatalf("staging no volume returned %v", err)
	}
}

func TestUnstageVolumeToleratesNothingMounted(t *testing.T) {
	chroot := t.TempDir()
	if err := unstageVolume(chroot); err != nil {
		t.Fatalf("unstaging an absent volume returned %v", err)
	}

	// And an ordinary file at the path, which is what a bind mount leaves
	// behind once it is detached. A second kill must not report an error.
	target := filepath.Join(chroot, BakedVolumePath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unstageVolume(chroot); err != nil {
		t.Fatalf("unstaging a plain file returned %v", err)
	}
}

func TestStageVolumeRejectsAMissingImage(t *testing.T) {
	err := stageVolume(t.TempDir(), filepath.Join(t.TempDir(), "gone.img"), 0, 0)
	if err == nil {
		t.Fatal("staging a volume image that does not exist succeeded")
	}
}

// DriveConfig reads the cache type back out of Firecracker rather than
// repeating what hostd meant to set, which is the only version of that check
// worth making.
func TestDriveConfigReadsTheLiveValue(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "fc.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vm/config" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"drives":[
			{"drive_id":"rootfs","path_on_host":"/srv/pilots/rootfs.ext4","is_root_device":true,"is_read_only":false,"cache_type":"Unsafe"},
			{"drive_id":"volume","path_on_host":"/srv/pilots/volume.img","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}
		]}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c := NewClient(sock)
	d, err := c.DriveConfig(context.Background(), VolumeDriveID)
	if err != nil {
		t.Fatalf("DriveConfig: %v", err)
	}
	if d.CacheType != CacheTypeWriteback {
		t.Fatalf("cache_type read back as %q, want %q", d.CacheType, CacheTypeWriteback)
	}
	if _, err := c.DriveConfig(context.Background(), "nope"); err == nil {
		t.Fatal("asking for a drive that is not attached returned no error")
	}
}
