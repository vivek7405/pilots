package machines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A cold boot cannot be driven to completion without Firecracker: nothing here
// can start a guest. What CAN be asserted without one is everything permanent
// -- which path the vendor decision took, what the row records afterwards, and
// what is deleted and when -- which is the part that survives the process and
// the part a mistake in would be silent. TestBootFromABuildPinsThatBuildAsThe
// DiskTemplate is the precedent for asserting row semantics this way.

// recordingStore wraps a store and keeps the order of the writes that matter.
type recordingStore struct {
	state.Store
	mu    sync.Mutex
	calls []string
}

func (s *recordingStore) note(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *recordingStore) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *recordingStore) PutMachine(ctx context.Context, m *state.Machine, opts ...state.WriteOption) error {
	s.note("PutMachine:" + m.State)
	return s.Store.PutMachine(ctx, m, opts...)
}

func (s *recordingStore) PutMachineCPU(ctx context.Context, c *state.MachineCPU, opts ...state.WriteOption) error {
	s.note("PutMachineCPU:" + c.LastStart)
	return s.Store.PutMachineCPU(ctx, c, opts...)
}

// deletingUploader records what a discard actually removed, in order.
type deletingUploader struct {
	mu      sync.Mutex
	deleted []string
}

func (u *deletingUploader) PutFile(context.Context, string, string) error { return nil }
func (u *deletingUploader) GetToFile(context.Context, string, string) error {
	return errors.New("absent")
}

func (u *deletingUploader) Delete(_ context.Context, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.deleted = append(u.deleted, key)
	return nil
}

func (u *deletingUploader) keys() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.deleted...)
}

func newColdBootManager(t *testing.T) (*Manager, *recordingStore, *deletingUploader) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	rec := &recordingStore{Store: st}
	up := &deletingUploader{}
	m := New(Options{
		HostID: "host-a", Store: rec, Vendor: "AuthenticAMD",
		Uploader: up, Chunks: up, CacheRoot: t.TempDir(), StateRoot: t.TempDir(),
	})
	return m, rec, up
}

// A machine that has never suspended has no memory image AND no disk in object
// storage, so there is nothing to cold-boot from. It stays the error it has
// always been, rather than becoming a boot from a template it never ran.
func TestANeverSnapshottedMachineIsNotColdBooted(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "m-1", Kind: state.KindMachine, Vendor: "GenuineIntel"}); err != nil {
		t.Fatal(err)
	}

	_, kind, err := m.bringUp(ctx, &state.Machine{ID: "m-1", VCPUs: 1, MemMiB: 512})
	if err == nil {
		t.Fatal("a machine with no memory build was brought up")
	}
	if !strings.Contains(err.Error(), "no usable memory build") {
		t.Fatalf("got %v, want the no-memory-build refusal", err)
	}
	if kind != "" {
		t.Errorf("a refusal reported a start kind of %q", kind)
	}
}

// The refusal that keeps golden-<vendor> from corrupting a cold boot.
//
// Both pools' rootfs bytes are identical, but each pool's first host
// chunkified its own copy and so minted a different build id. templateFor
// falls back to THIS host's pool template when the row names none, which would
// resolve a foreign machine's disk diff against the wrong parent -- silent
// corruption. So the row is refused instead.
func TestAColdBootRefusesARowWithNoTemplateDiskBuild(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "m-1", Kind: state.KindMachine, Vendor: "GenuineIntel"}); err != nil {
		t.Fatal(err)
	}

	_, kind, err := m.bringUp(ctx, &state.Machine{
		ID: "m-1", VCPUs: 1, MemMiB: 512, MemBuildID: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("a row naming no template disk build was cold-booted")
	}
	if !strings.Contains(err.Error(), "no template disk build") {
		t.Fatalf("got %v, want the missing-template-parent refusal", err)
	}
	if kind != state.StartColdBoot {
		t.Errorf("the vendor decision reported %q, want a cold boot", kind)
	}
}

// The other half of the decision: an image this host CAN load is restored, and
// the cold-boot path is not entered. Asserted through the error, because the
// restore reaches Firecracker and this one does not.
func TestASameVendorImageResumes(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	if err := m.opts.Store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: "m-1", Kind: state.KindMachine, Vendor: "AuthenticAMD"}); err != nil {
		t.Fatal(err)
	}

	// No template disk build, which the cold-boot path refuses by name. The
	// restore path does not care about it at this stage, so the error it fails
	// with says which path was taken.
	_, kind, err := m.bringUp(ctx, &state.Machine{
		ID: "m-1", VCPUs: 1, MemMiB: 512, MemBuildID: uuid.NewString(),
	})
	if kind != state.StartRestore {
		t.Fatalf("a same-vendor image reported %q, want a restore", kind)
	}
	if err != nil && strings.Contains(err.Error(), "no template disk build") {
		t.Fatalf("a same-vendor image took the cold-boot path: %v", err)
	}
}

// A machine with no recorded pool is one that predates this table. It ranks
// over the whole fleet and it RESTORES, which is exactly what it did before
// the table existed: an absent row is not a reason to reboot a guest.
func TestAnUnrecordedMachineStillRestores(t *testing.T) {
	m, _, _ := newColdBootManager(t)

	_, kind, err := m.bringUp(context.Background(), &state.Machine{
		ID: "m-1", VCPUs: 1, MemMiB: 512, MemBuildID: uuid.NewString(),
	})
	if kind != state.StartRestore {
		t.Fatalf("an unrecorded machine reported %q, want a restore", kind)
	}
	if err != nil && strings.Contains(err.Error(), "no template disk build") {
		t.Fatalf("an unrecorded machine took the cold-boot path: %v", err)
	}
}

// After a cold boot the machine keeps every disk pin -- the chain it just
// booted from is the chain its next suspend diffs against -- and loses the
// memory image, which is now wrong everywhere: the guest booted and its disk
// moved on, so resuming that image later on a same-vendor host would pair stale
// memory with a newer disk.
//
// The deletes run AFTER the row write, so nothing can read a row naming a
// build that is already gone.
func TestAColdBootDropsTheMemoryImageAfterTheRowWrite(t *testing.T) {
	m, rec, up := newColdBootManager(t)
	ctx := context.Background()

	memBuild := uuid.NewString()
	row := &state.Machine{
		ID: "m-1", Name: "alpha", HostID: "host-a", State: StateRunning,
		Domain: "alpha.pilotrun.app", VCPUs: 1, MemMiB: 512,
		MemBuildID:            memBuild,
		RootfsBuildID:         "rootfs-diff",
		TemplateRootfsBuildID: "rootfs-template",
		TemplateMemBuildID:    uuid.Nil.String(),
	}

	discard := m.recordStart(ctx, row, state.StartColdBoot)

	if row.MemBuildID != "" {
		t.Errorf("the superseded memory build is still on the row: %q", row.MemBuildID)
	}
	if row.RootfsBuildID != "rootfs-diff" || row.TemplateRootfsBuildID != "rootfs-template" {
		t.Errorf("a disk pin was lost: diff=%q template=%q",
			row.RootfsBuildID, row.TemplateRootfsBuildID)
	}
	if row.Domain != "alpha.pilotrun.app" {
		t.Errorf("the URL moved: %q", row.Domain)
	}
	if got := up.keys(); len(got) != 0 {
		t.Fatalf("objects were deleted before the row was written: %v", got)
	}

	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}
	discard()

	want := map[string]bool{
		memBuild + "/header":  true,
		memBuild + "/data":    true,
		suspendSnapKey("m-1"): true,
		prefetchKey("m-1"):    true,
	}
	got := up.keys()
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want exactly %d keys", got, len(want))
	}
	for _, key := range got {
		if !want[key] {
			t.Errorf("deleted an object nothing superseded: %q", key)
		}
	}

	// And the pool the machine is now in was written before the row that names
	// it running, so no peer ranks its next rescue from a stale vendor.
	order := rec.order()
	if len(order) < 2 || order[0] != "PutMachineCPU:cold_boot" || order[1] != "PutMachine:running" {
		t.Fatalf("write order was %v, want the cpu row before the machine row", order)
	}

	cpu, err := m.opts.Store.GetMachineCPU(ctx, "m-1")
	if err != nil {
		t.Fatalf("no start was recorded: %v", err)
	}
	if cpu.Vendor != "AuthenticAMD" || cpu.LastStart != state.StartColdBoot || cpu.LastStartAt == 0 {
		t.Errorf("recorded %+v, want a cold boot on this host's vendor", *cpu)
	}
}

// A restore records the start too, and touches nothing else: the memory image
// it just resumed is still the right one.
func TestARestoreRecordsItsStartAndKeepsTheMemoryImage(t *testing.T) {
	m, _, up := newColdBootManager(t)
	ctx := context.Background()

	row := &state.Machine{ID: "m-1", HostID: "host-a", MemBuildID: "mem-1", VCPUs: 1, MemMiB: 512}
	m.recordStart(ctx, row, state.StartRestore)()

	if row.MemBuildID != "mem-1" {
		t.Errorf("a restore dropped the memory image: %q", row.MemBuildID)
	}
	if got := up.keys(); len(got) != 0 {
		t.Fatalf("a restore deleted %v", got)
	}
	cpu, err := m.opts.Store.GetMachineCPU(ctx, "m-1")
	if err != nil {
		t.Fatalf("no start was recorded: %v", err)
	}
	if cpu.LastStart != state.StartRestore {
		t.Errorf("recorded %q, want a restore", cpu.LastStart)
	}
}

// A cold boot re-claims the volume the machine already holds, for the SAME
// machine id. claimVolume refuses a volume held by a different machine, so
// minting a new id here is the one way this could break -- and it would break
// exactly on the machines that can least afford it, the ones with data.
func TestAColdBootReclaimsItsOwnVolume(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	fv := &fakeVolumes{store: m.opts.Store}
	m.opts.Volumes = fv
	if err := m.opts.Store.PutVolume(ctx, &state.Volume{
		ID: "vol-1", MachineID: "m-1", HostID: "host-a", MountPath: "/data",
	}); err != nil {
		t.Fatal(err)
	}

	row := &state.Machine{
		ID: "m-1", HostID: "host-a", VCPUs: 1, MemMiB: 512,
		VolumeID:              "vol-1",
		TemplateRootfsBuildID: uuid.Nil.String(),
		TemplateMemBuildID:    uuid.Nil.String(),
	}
	// The boot itself needs Firecracker. What is asserted is that the claim
	// happened first, and with this machine's own id.
	_, _ = m.bootFromDisk(ctx, row, fc.Backends{}, "")

	if len(fv.attached) != 1 || fv.attached[0] != "vol-1" {
		t.Fatalf("the volume was not re-attached: %v", fv.attached)
	}
	got, err := m.opts.Store.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != "m-1" {
		t.Fatalf("the volume row names machine %q, want m-1", got.MachineID)
	}
	if got.HostID != "host-a" {
		t.Fatalf("the volume row names host %q, want host-a", got.HostID)
	}
}

// A rollback whose CHECKPOINT was photographed on the other vendor takes the
// same downgrade a wake takes. Asserted through the refusal a row with no
// template disk build produces, which only the cold-boot path emits.
func TestAForeignCheckpointBootsFromItsDisk(t *testing.T) {
	m, _, _ := newColdBootManager(t)
	ctx := context.Background()

	ckpt := &state.Checkpoint{ID: "ck-1", MachineID: "m-1", MemBuildID: uuid.NewString()}
	if err := m.opts.Store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: ckpt.ID, Kind: state.KindCheckpoint, Vendor: "GenuineIntel"}); err != nil {
		t.Fatal(err)
	}
	// Durable, because a cold boot serves the checkpoint's rootfs build over
	// NBD from object storage exactly as a restore does, and waits for the same
	// upload. Marked here so the wait is not what this test measures.
	dir := m.checkpointDir("m-1", ckpt.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".durable"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	row := &state.Machine{ID: "m-1", HostID: "host-a", VCPUs: 1, MemMiB: 512}
	_, kind, err := m.restoreFromCheckpoint(ctx, row, ckpt)
	if kind != state.StartColdBoot {
		t.Fatalf("a foreign checkpoint reported %q, want a cold boot", kind)
	}
	if err == nil || !strings.Contains(err.Error(), "no template disk build") {
		t.Fatalf("got %v, want the cold-boot path's refusal", err)
	}
}

// A create knows its own start kind from the request, and writes it BEFORE the
// machine row -- mirroring tenancy, and for the same shape of reason: a peer
// that saw a machine whose pool is unknown would rank its rescue over the whole
// fleet and could cold-boot it needlessly.
func TestAStartIsRecordedBeforeTheMachineRow(t *testing.T) {
	m, rec, _ := newColdBootManager(t)
	ctx := context.Background()

	row := &state.Machine{ID: "m-1", HostID: "host-a", State: StateCreating, VCPUs: 1, MemMiB: 512}
	m.recordStart(ctx, row, state.StartBoot)
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		t.Fatal(err)
	}

	order := rec.order()
	if len(order) < 2 || order[0] != "PutMachineCPU:boot" || order[1] != "PutMachine:creating" {
		t.Fatalf("write order was %v, want the cpu row first", order)
	}
}
