// Package volumes owns persistent per-write-durable storage.
//
// The shape, and why it is this shape:
//
// A volume is one JuiceFS filesystem whose data lives in the same object
// store as everything else, holding a single raw ext4 image that the guest
// gets as a second virtio-blk drive. Firecracker has no virtio-fs -- the
// upstream request is open and the p9 implementation was rejected on security
// grounds -- so there is no version of this where the guest sees the
// filesystem directly.
//
// The metadata engine is a SQLite file on local disk, replicated to object
// storage by Litestream. That is deliberately NOT a shared Redis or Postgres:
// a fleet-wide metadata service is a central control plane, which the
// architecture forbids, and it would make every volume in the fleet
// unavailable when one box dies. SQLite's single-writer constraint is a
// feature here rather than a limitation, because it enforces the property the
// volume needs anyway -- exactly one host mounts a volume at a time, the host
// running its machine.
package volumes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Layout on a host. The defaults are what scripts/host-bootstrap.sh creates
// and what the litestream@ template unit names, so a host and its units agree
// without either being told; a Manager can be pointed elsewhere, which is what
// the tests do.
const (
	// DefaultMetaRoot holds each volume's SQLite metadata database.
	DefaultMetaRoot = "/var/lib/pilot-volumes"
	// DefaultMountRoot is where each volume's JuiceFS filesystem is mounted.
	DefaultMountRoot = "/mnt/pilot-volumes"
	// DefaultCacheRoot is JuiceFS's local block cache. Purely a cache:
	// deleting it costs re-reads from object storage, never data.
	DefaultCacheRoot = "/var/cache/pilot-volumes"
	// DefaultConfigRoot holds the per-volume Litestream configuration.
	DefaultConfigRoot = "/etc/pilots/litestream"
)

// ImageName is the volume's disk image, the file the guest actually gets.
// One per volume, at a fixed name inside the JuiceFS mount.
const ImageName = "disk.img"

// DefaultMountPath is where the guest mounts the volume when the caller says
// nothing.
const DefaultMountPath = "/data"

// bucketPrefix is where volume data lives in the bucket. Both JuiceFS's chunks
// and Litestream's metadata replicas land under it, one directory per volume,
// so a volume is one prefix to inspect and one prefix to delete.
const bucketPrefix = "volumes"

// Config is what a Manager needs to place and reach volumes.
type Config struct {
	HostID string

	// Object storage, the same bucket and credentials the rest of hostd uses.
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string

	// Binaries, installed by scripts/host-bootstrap.sh.
	JuiceFSBin    string
	LitestreamBin string

	// CacheSizeMiB bounds the local block cache per volume.
	CacheSizeMiB int

	// Where volumes live on this host. Empty means the Default* above.
	MetaRoot   string
	MountRoot  string
	CacheRoot  string
	ConfigRoot string
}

// runner executes an external command.
//
// A field rather than a direct exec call so that tests can assert the exact
// argv. The flags ARE the behaviour here -- a volume mounted with one extra
// flag is a volume that loses the last few seconds of writes when its host
// dies -- and there is no way to check that on a machine without JuiceFS
// installed except by looking at what would have been run.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "),
			err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Manager creates, mounts and moves this host's volumes.
type Manager struct {
	cfg Config
	run runner
}

// New returns a Manager that shells out to the real binaries.
func New(cfg Config) *Manager {
	if cfg.JuiceFSBin == "" {
		cfg.JuiceFSBin = "/opt/pilots/bin/juicefs"
	}
	if cfg.LitestreamBin == "" {
		cfg.LitestreamBin = "/opt/pilots/bin/litestream"
	}
	if cfg.CacheSizeMiB == 0 {
		cfg.CacheSizeMiB = 20480
	}
	for _, d := range []struct {
		field *string
		def   string
	}{
		{&cfg.MetaRoot, DefaultMetaRoot},
		{&cfg.MountRoot, DefaultMountRoot},
		{&cfg.CacheRoot, DefaultCacheRoot},
		{&cfg.ConfigRoot, DefaultConfigRoot},
	} {
		if *d.field == "" {
			*d.field = d.def
		}
	}
	return &Manager{cfg: cfg, run: execRunner}
}

// NewID mints a volume id.
//
// Hyphen-separated for the same reason a machine id is: it ends up in a
// filesystem name, a systemd unit instance and an object-storage prefix, and
// something that is safe everywhere needs sanitising nowhere.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("volumes: entropy unavailable: %v", err))
	}
	return "vol-" + hex.EncodeToString(b)
}

// MetaPath is the volume's SQLite metadata database.
func (m *Manager) MetaPath(id string) string {
	return filepath.Join(m.cfg.MetaRoot, id, "meta.db")
}

// MountPoint is where the volume's filesystem is mounted on this host.
func (m *Manager) MountPoint(id string) string { return filepath.Join(m.cfg.MountRoot, id) }

// ImagePath is the raw ext4 image the guest gets as its second drive. It lives
// INSIDE the JuiceFS mount, which is what makes a guest write durable in
// object storage rather than durable on one host's disk.
func (m *Manager) ImagePath(id string) string { return filepath.Join(m.MountPoint(id), ImageName) }

// CacheDir is the volume's local block cache.
func (m *Manager) CacheDir(id string) string { return filepath.Join(m.cfg.CacheRoot, id) }

// ConfigPath is the volume's Litestream configuration.
func (m *Manager) ConfigPath(id string) string {
	return filepath.Join(m.cfg.ConfigRoot, id+".yml")
}

// S3Prefix is everything belonging to this volume in the bucket.
func S3Prefix(id string) string { return bucketPrefix + "/" + id + "/" }

// metaReplicaPath is where Litestream keeps the metadata database's replica.
// Under the volume's own prefix, so one volume is one prefix.
func metaReplicaPath(id string) string { return bucketPrefix + "/" + id + "/meta" }

// bucketURL is what JuiceFS is told to write its chunks into.
//
// Path-style, per the architecture's storage rule: most non-AWS S3
// implementations do not serve virtual-host style, and the fleet's bucket is
// one of them.
func (m *Manager) bucketURL() string {
	endpoint := strings.TrimSuffix(m.cfg.Endpoint, "/")
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	return endpoint + "/" + m.cfg.Bucket + "/" + bucketPrefix
}

// ensureDirs creates the per-volume directories on this host.
func (m *Manager) ensureDirs(id string) error {
	for _, dir := range []string{
		filepath.Dir(m.MetaPath(id)), m.MountPoint(id), m.CacheDir(id), m.cfg.ConfigRoot,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("volumes: mkdir %s: %w", dir, err)
		}
	}
	return nil
}
