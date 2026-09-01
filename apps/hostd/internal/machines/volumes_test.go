package machines

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fakeVolumes records what the lifecycle asked of the volume layer, without
// needing JuiceFS.
type fakeVolumes struct {
	created  int
	attached []string
	detached []string
	// attachedWhileOwned records who the row said owned the volume at the
	// moment Attach was called. Mounting before the row claims ownership is
	// the window in which two hosts can both mount one metadata database.
	ownerAtAttach []string
	store         state.Store
}

func (f *fakeVolumes) Create(_ context.Context, name string, sizeMiB int, mountPath string) (*state.Volume, error) {
	f.created++
	return &state.Volume{
		ID: "vol-1", Name: name, SizeMiB: sizeMiB, MountPath: mountPath,
		HostID: "host-a", S3Prefix: "volumes/vol-1/",
	}, nil
}

func (f *fakeVolumes) Attach(ctx context.Context, v *state.Volume) error {
	f.attached = append(f.attached, v.ID)
	if row, err := f.store.GetVolume(ctx, v.ID); err == nil {
		f.ownerAtAttach = append(f.ownerAtAttach, row.HostID)
	} else {
		f.ownerAtAttach = append(f.ownerAtAttach, "<no row>")
	}
	return nil
}

func (f *fakeVolumes) Detach(_ context.Context, id string) error {
	f.detached = append(f.detached, id)
	return nil
}

func (f *fakeVolumes) ImagePath(id string) string { return "/mnt/pilot-volumes/" + id + "/disk.img" }

func newVolumeTestManager(t *testing.T) (*Manager, *fakeVolumes, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	fv := &fakeVolumes{store: st}
	return New(Options{HostID: "host-a", Store: st, Volumes: fv}), fv, st
}

func TestCreateVolumeWritesTheRowAfterTheVolumeExists(t *testing.T) {
	ctx := context.Background()
	m, fv, st := newVolumeTestManager(t)

	v, err := m.CreateVolume(ctx, api.CreateVolumeRequest{Name: "data", SizeGiB: 4, MountPath: "/data"})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if fv.created != 1 {
		t.Fatal("the volume layer was never asked to create anything")
	}
	if v.SizeMiB != 4096 {
		t.Errorf("size is %d MiB, want 4096", v.SizeMiB)
	}

	got, err := st.GetVolume(ctx, v.ID)
	if err != nil {
		t.Fatalf("the row was not published: %v", err)
	}
	if got.HostID != "host-a" || got.MountPath != "/data" {
		t.Fatalf("row is %+v", *got)
	}
}

func TestCreateVolumeRefusedWithoutAVolumeLayer(t *testing.T) {
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m := New(Options{HostID: "host-a", Store: st})
	if _, err := m.CreateVolume(context.Background(),
		api.CreateVolumeRequest{Name: "data", SizeGiB: 4}); !errors.Is(err, ErrNoVolumes) {
		t.Fatalf("got %v, want ErrNoVolumes", err)
	}
}

// The row is claimed before the filesystem is mounted. Mounting first opens
// exactly the window the ownership row exists to close: two hosts with the
// same JuiceFS metadata database open, which does not error and does not
// recover.
func TestClaimVolumeTakesTheRowBeforeMounting(t *testing.T) {
	ctx := context.Background()
	m, fv, st := newVolumeTestManager(t)

	if err := st.PutVolume(ctx, &state.Volume{ID: "vol-1", SizeMiB: 1024, MountPath: "/data"}); err != nil {
		t.Fatal(err)
	}

	v, err := m.claimVolume(ctx, "vol-1", "m-1")
	if err != nil {
		t.Fatalf("claimVolume: %v", err)
	}
	if v.HostID != "host-a" || v.MachineID != "m-1" {
		t.Fatalf("claim returned %+v", *v)
	}
	if len(fv.ownerAtAttach) != 1 || fv.ownerAtAttach[0] != "host-a" {
		t.Fatalf("the volume was mounted while the row said the owner was %v; "+
			"another host could still have claimed it", fv.ownerAtAttach)
	}

	row, err := st.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.MachineID != "m-1" {
		t.Fatalf("row machine_id is %q", row.MachineID)
	}
}

// One machine per volume. Two machines sharing one image would corrupt the
// filesystem inside it as surely as two hosts sharing the metadata database.
func TestClaimVolumeRefusesOneAlreadyAttached(t *testing.T) {
	ctx := context.Background()
	m, fv, st := newVolumeTestManager(t)

	if err := st.PutVolume(ctx, &state.Volume{ID: "vol-1", MachineID: "m-other"}); err != nil {
		t.Fatal(err)
	}
	_, err := m.claimVolume(ctx, "vol-1", "m-1")
	if err == nil || !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("got %v, want a refusal naming the other machine", err)
	}
	if len(fv.attached) != 0 {
		t.Fatal("the volume was mounted anyway")
	}
}

// Re-claiming a volume this machine already holds is the ordinary wake path
// and must not be an error.
func TestClaimVolumeIsIdempotentForItsOwnMachine(t *testing.T) {
	ctx := context.Background()
	m, _, st := newVolumeTestManager(t)

	if err := st.PutVolume(ctx, &state.Volume{ID: "vol-1", MachineID: "m-1", HostID: "host-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.claimVolume(ctx, "vol-1", "m-1"); err != nil {
		t.Fatalf("re-claiming our own volume: %v", err)
	}
}

// A destroyed machine's volume goes back to being unattached even if the
// unmount failed: a volume still marked as this host's is a volume no other
// host will ever mount.
func TestReleaseVolumeClearsTheRow(t *testing.T) {
	ctx := context.Background()
	m, fv, st := newVolumeTestManager(t)

	if err := st.PutVolume(ctx, &state.Volume{ID: "vol-1", MachineID: "m-1", HostID: "host-a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.releaseVolume(ctx, "vol-1"); err != nil {
		t.Fatalf("releaseVolume: %v", err)
	}
	if len(fv.detached) != 1 {
		t.Fatalf("the volume layer was not asked to detach: %v", fv.detached)
	}
	row, err := st.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.MachineID != "" {
		t.Fatalf("row still names machine %q", row.MachineID)
	}
}

func TestReleaseVolumeIsANoOpWithoutOne(t *testing.T) {
	m, fv, _ := newVolumeTestManager(t)
	if err := m.releaseVolume(context.Background(), ""); err != nil {
		t.Fatalf("releasing no volume returned %v", err)
	}
	if len(fv.detached) != 0 {
		t.Fatal("something was detached")
	}
}

// The image path reaches Firecracker's config from one place, so neither the
// boot path nor the restore path can forget it.
func TestMachineConfigCarriesTheVolumeImage(t *testing.T) {
	m, fv, _ := newVolumeTestManager(t)

	withVolume := m.machineFCConfig(&state.Machine{ID: "m-1", VolumeID: "vol-1", VCPUs: 1, MemMiB: 512}, nil, "")
	if withVolume.VolumeImage != fv.ImagePath("vol-1") {
		t.Fatalf("VolumeImage is %q, want %q", withVolume.VolumeImage, fv.ImagePath("vol-1"))
	}

	without := m.machineFCConfig(&state.Machine{ID: "m-2", VCPUs: 1, MemMiB: 512}, nil, "")
	if without.VolumeImage != "" {
		t.Fatalf("a machine with no volume was given the image %q", without.VolumeImage)
	}
}
