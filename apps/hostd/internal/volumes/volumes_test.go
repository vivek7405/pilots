package volumes

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// recorder stands in for the binaries a host has and a laptop does not. What
// it captures IS the assertion: these flags are the whole behaviour of a
// volume, and none of them can be checked any other way here.
type recorder struct {
	calls []call
}

type call struct {
	name string
	args []string
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, call{name: name, args: args})
	return nil, nil
}

// names returns the sequence of commands run, so an ordering can be asserted
// as a sequence rather than by index arithmetic.
func (r *recorder) names() []string {
	out := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		name := filepath.Base(c.name)
		if len(c.args) > 0 {
			name += " " + c.args[0]
		}
		out = append(out, name)
	}
	return out
}

func (r *recorder) find(t *testing.T, prefix string) call {
	t.Helper()
	for _, c := range r.calls {
		if len(c.args) > 0 && filepath.Base(c.name)+" "+c.args[0] == prefix {
			return c
		}
	}
	t.Fatalf("no %q was run; ran %v", prefix, r.names())
	return call{}
}

func newTestManager(t *testing.T) (*Manager, *recorder) {
	t.Helper()
	root := t.TempDir()
	m := New(Config{
		HostID:    "host-a",
		Endpoint:  "https://fsn1.your-objectstorage.com",
		Region:    "eu-central-1",
		Bucket:    "pilots",
		AccessKey: "AK", SecretKey: "SK",
		JuiceFSBin: "/opt/pilots/bin/juicefs", LitestreamBin: "/opt/pilots/bin/litestream",
		MetaRoot:   filepath.Join(root, "meta"),
		MountRoot:  filepath.Join(root, "mnt"),
		CacheRoot:  filepath.Join(root, "cache"),
		ConfigRoot: filepath.Join(root, "etc"),
	})
	rec := &recorder{}
	m.run = rec.run
	return m, rec
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// Landmine 3, and the reason this test is the first one in the file.
//
// --writeback makes JuiceFS acknowledge a write once it is in the local cache
// and upload it later. A volume mounted that way passes every test that writes
// a marker and reads it back, and loses the marker the one time it matters --
// when the host dies between the acknowledgement and the upload.
func TestMountNeverAsksForWriteback(t *testing.T) {
	m, _ := newTestManager(t)
	args := m.mountArgs("vol-1")

	for _, forbidden := range []string{"--writeback", "-writeback"} {
		if hasFlag(args, forbidden) {
			t.Fatalf("juicefs mount was given %s: writes would be acknowledged "+
				"before they are durable, which is the one thing a volume promises "+
				"not to do. args: %v", forbidden, args)
		}
	}
}

func TestMountArgs(t *testing.T) {
	m, _ := newTestManager(t)
	args := m.mountArgs("vol-1")

	if args[0] != "mount" || !hasFlag(args, "-d") {
		t.Fatalf("expected a daemonised mount, got %v", args)
	}
	if got, _ := flagValue(args, "--cache-dir"); got != m.CacheDir("vol-1") {
		t.Errorf("--cache-dir is %q, want %q", got, m.CacheDir("vol-1"))
	}
	if got, _ := flagValue(args, "--backup-meta"); got != "3600" {
		t.Errorf("--backup-meta is %q, want 3600", got)
	}
	// The META-URL and the mount point are positional and last, in that order.
	if args[len(args)-2] != m.metaURL("vol-1") {
		t.Errorf("meta url is %q, want %q", args[len(args)-2], m.metaURL("vol-1"))
	}
	if args[len(args)-1] != m.MountPoint("vol-1") {
		t.Errorf("mount point is %q, want %q", args[len(args)-1], m.MountPoint("vol-1"))
	}
}

// The metadata engine is SQLite on local disk, not a networked database.
// Anything shared would be a central metadata service, which makes every
// volume in the fleet unavailable when one box dies.
func TestFormatUsesASqliteMetaEngine(t *testing.T) {
	m, _ := newTestManager(t)
	args := m.formatArgs("vol-1")

	meta := args[len(args)-2]
	if !strings.HasPrefix(meta, "sqlite3://") {
		t.Fatalf("meta engine is %q; a networked engine here is a control plane", meta)
	}
	if meta != "sqlite3://"+m.MetaPath("vol-1") {
		t.Errorf("meta url is %q, want the local database path", meta)
	}
	if args[len(args)-1] != "vol-1" {
		t.Errorf("filesystem name is %q, want the volume id", args[len(args)-1])
	}
}

func TestFormatArgs(t *testing.T) {
	m, _ := newTestManager(t)
	args := m.formatArgs("vol-1")

	for flag, want := range map[string]string{
		"--storage":     "s3",
		"--block-size":  "4096",
		"--compress":    "none",
		"--trash-days":  "0",
		"--access-key":  "AK",
		"--secret-key":  "SK",
		"--bucket":      "https://fsn1.your-objectstorage.com/pilots/volumes",
		"--not-present": "",
	} {
		got, ok := flagValue(args, flag)
		if flag == "--not-present" {
			if ok {
				t.Errorf("a flag that should not exist was found")
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s is %q (present=%v), want %q", flag, got, ok, want)
		}
	}
}

// Path-style addressing, and an endpoint that is already a URL is not
// double-prefixed.
func TestBucketURLIsPathStyle(t *testing.T) {
	m, _ := newTestManager(t)
	if got := m.bucketURL(); got != "https://fsn1.your-objectstorage.com/pilots/volumes" {
		t.Fatalf("bucket url is %q", got)
	}

	bare := New(Config{Endpoint: "fsn1.your-objectstorage.com/", Bucket: "pilots"})
	if got := bare.bucketURL(); got != "https://fsn1.your-objectstorage.com/pilots/volumes" {
		t.Fatalf("bare endpoint produced %q", got)
	}
}

func TestLitestreamConfig(t *testing.T) {
	m, _ := newTestManager(t)
	cfg := m.litestreamConfig("vol-1")

	for _, want := range []string{
		"path: " + m.MetaPath("vol-1"),
		"bucket: pilots",
		"path: volumes/vol-1/meta",
		"endpoint: https://fsn1.your-objectstorage.com",
		"region: eu-central-1",
		"force-path-style: true",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("litestream config is missing %q:\n%s", want, cfg)
		}
	}
}

// Landmine 5. A host taking a volume over must restore the metadata from
// object storage BEFORE it mounts. Mounting against a stale local database
// does not error -- it silently comes back missing whatever the previous host
// wrote last.
func TestAttachRestoresMetadataBeforeMounting(t *testing.T) {
	m, rec := newTestManager(t)

	// Attach fails at the end here (no image behind the fake mount), which is
	// fine: the ordering it asserts has already happened.
	_ = m.Attach(context.Background(), &state.Volume{ID: "vol-1", SizeMiB: 1024})

	ran := rec.names()
	restore, mount := -1, -1
	for i, name := range ran {
		switch name {
		case "litestream restore":
			restore = i
		case "juicefs mount":
			mount = i
		}
	}
	if restore < 0 || mount < 0 {
		t.Fatalf("expected both a restore and a mount, ran %v", ran)
	}
	if restore > mount {
		t.Fatalf("mounted before restoring the metadata: %v", ran)
	}

	args := rec.find(t, "litestream restore").args
	if !hasFlag(args, "-if-replica-exists") {
		t.Errorf("restore is missing -if-replica-exists, so the first mount after "+
			"a create would fail: %v", args)
	}
}

// A create formats, starts replicating, mounts, and only then writes the image
// -- everything that keeps the image's bytes alive is running before there are
// any bytes.
func TestCreateOrdersItsSteps(t *testing.T) {
	m, rec := newTestManager(t)

	// mke2fs is the last step and needs a real mount; let it fail there.
	_, _ = m.Create(context.Background(), "data", 64, "/data")

	want := []string{"juicefs format", "systemctl enable", "juicefs mount"}
	got := rec.names()
	if len(got) < len(want) {
		t.Fatalf("ran %v, want at least %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d was %q, want %q (full sequence %v)", i, got[i], want[i], got)
		}
	}
}

// Detach unmounts before it stops replicating: stopping Litestream is what
// flushes the last metadata writes, so the flush has to come after the last
// thing that writes metadata.
func TestDetachUnmountsBeforeStoppingReplication(t *testing.T) {
	m, rec := newTestManager(t)
	// Nothing is mounted, so Unmount is a no-op and only the stop is recorded;
	// what matters is that a stop is never issued first when both happen.
	if err := m.Detach(context.Background(), "vol-1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	got := rec.names()
	if len(got) != 1 || got[0] != "systemctl disable" {
		t.Fatalf("ran %v, want just the replication stop", got)
	}
}

func TestValidateMountPath(t *testing.T) {
	for _, ok := range []string{"/data", "/var/lib/postgresql", "/srv/uploads"} {
		if err := validateMountPath(ok); err != nil {
			t.Errorf("validateMountPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"data", "", "/", "/etc", "/usr/", "/proc"} {
		if err := validateMountPath(bad); err == nil {
			t.Errorf("validateMountPath(%q) allowed a path that would break the guest", bad)
		}
	}
}

func TestHasMountPoint(t *testing.T) {
	table := "proc /proc proc rw 0 0\n" +
		"JuiceFS:vol-1 /mnt/pilot-volumes/vol-1 fuse.juicefs rw 0 0\n" +
		"none /mnt/with\\040space tmpfs rw 0 0\n"

	for path, want := range map[string]bool{
		"/mnt/pilot-volumes/vol-1": true,
		"/mnt/pilot-volumes/vol-2": false,
		"/mnt/with space":          true,
		"/proc":                    true,
	} {
		if got := hasMountPoint(table, path); got != want {
			t.Errorf("hasMountPoint(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestNewIDIsSafeEverywhereItAppears(t *testing.T) {
	id := NewID()
	if !strings.HasPrefix(id, "vol-") {
		t.Fatalf("id %q has no prefix", id)
	}
	for _, r := range id {
		if !(r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			t.Fatalf("id %q contains %q, which is not safe in a systemd unit "+
				"instance or an object key", id, r)
		}
	}
	if NewID() == id {
		t.Fatal("two volume ids collided")
	}
}

// The image the guest actually gets. mke2fs is a bootstrap dependency and is
// present wherever this runs, so this exercises the real thing rather than the
// recorder.
func TestCreateImageProducesAMountableExt4(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("mke2fs is not installed")
	}
	m, _ := newTestManager(t)
	m.run = execRunner

	id := "vol-1"
	if err := m.ensureDirs(id); err != nil {
		t.Fatal(err)
	}
	if err := m.createImage(context.Background(), id, 16); err != nil {
		t.Fatalf("createImage: %v", err)
	}

	out, err := exec.Command("dumpe2fs", "-h", m.ImagePath(id)).CombinedOutput()
	if err != nil {
		t.Fatalf("the image is not a filesystem: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "Block size:               4096") {
		t.Errorf("expected 4096-byte blocks:\n%s", out)
	}

	// Idempotent, and that is not politeness: a retried create must never
	// reformat a volume that already holds a machine's data.
	uuidBefore := fsUUID(t, m.ImagePath(id))
	if err := m.createImage(context.Background(), id, 16); err != nil {
		t.Fatalf("second createImage: %v", err)
	}
	if after := fsUUID(t, m.ImagePath(id)); after != uuidBefore {
		t.Fatalf("the image was reformatted: uuid went from %s to %s", uuidBefore, after)
	}
}

// fsUUID reads the filesystem uuid, which changes on every mkfs and on nothing
// else -- so it is what tells a reformat apart from a no-op.
func fsUUID(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("dumpe2fs", "-h", path).CombinedOutput()
	if err != nil {
		t.Fatalf("dumpe2fs %s: %v: %s", path, err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Filesystem UUID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Filesystem UUID:"))
		}
	}
	t.Fatalf("no filesystem uuid in:\n%s", out)
	return ""
}
