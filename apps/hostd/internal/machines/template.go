package machines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Creating a machine IS a restore. The template is a machine that was booted
// once, allowed to settle, and snapshotted; every machine after that starts
// from its memory and its disk rather than from a kernel boot. That is what
// turns a twenty-second boot into a sub-second create, and it means the create
// path and the wake path are the same code.
//
// The template is built once per host, on demand, and cached.

// templateFile records which builds the golden template produced.
const templateFile = "template.json"

// settleTime is how long a freshly booted guest is given before its memory is
// captured.
//
// A snapshot taken earlier catches a half-converged systemd: the machine
// restores, accepts connections, and never finishes servicing them, because
// the units that would have finished starting were frozen mid-transition.
const settleTime = 20 * time.Second

// Template is the golden image every machine is created from.
type Template struct {
	MemBuildID    uuid.UUID `json:"mem_build_id"`
	RootfsBuildID uuid.UUID `json:"rootfs_build_id"`
	SnapKey       string    `json:"snap_key"`
	CreatedAt     int64     `json:"created_at"`
	// PageSizeKiB is the guest page size baked into MemBuildID. Firecracker
	// cannot restore a 2MiB memory image into a 4KiB VM or the reverse, so a
	// manifest that disagrees with this host's setting is not stale, it is
	// unusable -- the template is rebuilt rather than repaired. Zero means a
	// manifest written before page size was recorded, which is treated the
	// same way.
	PageSizeKiB int `json:"page_size_kib"`
}

// pageSizeKiB is the guest page size this host photographs templates at.
func (m *Manager) pageSizeKiB() int {
	if m.opts.FCConfig.HugePages {
		return 2048
	}
	return 4
}

// templateRoot is where the template's builds and manifest live on this host.
func (m *Manager) templateRoot() string { return filepath.Join(m.opts.CacheRoot, "template") }

// buildDir is where chunkify writes builds before they are uploaded, and where
// a restore reads a local template from.
func (m *Manager) buildDir() string { return filepath.Join(m.opts.CacheRoot, "builds") }

// memParentDir is the template's memory build, which every machine's memory
// snapshot is diffed against. A machine that changed a tenth of its memory
// stores a tenth of its memory.
func (m *Manager) memParentDir(t *Template) string {
	// A machine that was BOOTED rather than restored has no memory parent, and
	// must not be given one: its pages are not a divergence from the
	// template's photographed memory, so recording the identical ones as
	// pointers into that image would resolve them from a different machine.
	if t.MemBuildID == uuid.Nil {
		return ""
	}
	return filepath.Join(m.buildDir(), t.MemBuildID.String())
}

// rootfsTemplateDir is the template's disk build, which every machine's disk
// reads through.
func (m *Manager) rootfsTemplateDir(t *Template) string {
	return filepath.Join(m.buildDir(), t.RootfsBuildID.String())
}

// templateOnce serialises template construction. Two concurrent creates on a
// cold host would otherwise each boot their own template machine, and the
// second would overwrite the first's manifest while machines were already
// restoring against it.
var templateOnce sync.Mutex

// EnsureTemplate returns the golden template, adopting the fleet's if there is
// one and building it if there is not.
//
// The template is FLEET-WIDE, and it has to be. A machine's memory image is a
// diff against the template it was created from, so a host that built its own
// could not restore anyone else's machines -- which is the whole of rescue.
// The order is therefore: what this host already has, then what the fleet
// says, then build one.
func (m *Manager) EnsureTemplate(ctx context.Context) (*Template, error) {
	templateOnce.Lock()
	defer templateOnce.Unlock()

	// Already local and complete.
	if t, err := m.loadTemplate(); err == nil {
		return t, nil
	}

	// The fleet has one; pull what this host is missing.
	if t, err := m.adoptFleetTemplate(ctx); err == nil {
		return t, nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	slog.Info("no golden template in this vendor pool; building one", "vendor", m.opts.Vendor)
	start := time.Now()

	t, err := m.buildTemplate(ctx)
	if err != nil {
		return nil, err
	}

	// Publish before saving locally, then re-read. Two fresh hosts can each
	// find no template and each build one; last write wins the row, and the
	// loser must adopt the winner's rather than keep serving from a template
	// no other host can restore against.
	if err := m.opts.Store.PutTemplate(ctx, &state.Template{
		ID: state.GoldenTemplateFor(m.opts.Vendor), MemBuildID: t.MemBuildID.String(),
		RootfsBuildID: t.RootfsBuildID.String(), SnapKey: t.SnapKey,
		CreatedAt: t.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("machines: publish the golden template: %w", err)
	}

	if winner, err := m.adoptFleetTemplate(ctx); err == nil {
		if winner.MemBuildID != t.MemBuildID {
			slog.Info("another host of this vendor published a golden template first; adopting it",
				"vendor", m.opts.Vendor, "ours", t.MemBuildID, "theirs", winner.MemBuildID)
		}
		return winner, nil
	}

	if err := m.saveTemplate(t); err != nil {
		return nil, err
	}
	slog.Info("golden template ready", "vendor", m.opts.Vendor,
		"seconds", int(time.Since(start).Seconds()),
		"mem_build", t.MemBuildID, "rootfs_build", t.RootfsBuildID)
	return t, nil
}

// adoptFleetTemplate makes the fleet's template usable on this host.
//
// The builds are content-addressed and already in object storage, so adopting
// one is a download rather than a rebuild: open each build remotely, pull it
// in full, and it lands in exactly the layout a local build directory has.
// That is what lets a host that has never run a template restore a machine
// created from it.
func (m *Manager) adoptFleetTemplate(ctx context.Context) (*Template, error) {
	// One row per vendor pool. The rootfs bytes are identical fleet-wide, but
	// the memory half is a Firecracker snapshot and never restores across the
	// Intel/AMD boundary, so a host adopts its OWN pool's row and nobody
	// else's.
	row, err := m.opts.Store.GetTemplate(ctx, state.GoldenTemplateFor(m.opts.Vendor))
	if err != nil {
		return nil, err
	}

	// Stamped with THIS host's page size, because the row does not carry one
	// and the setting is fleet-wide by construction: every host photographs
	// at the same size or none of them can restore each other's machines
	// (see fc.HugePages2M). If a host is misprovisioned the restore fails
	// loudly on the snapshot itself, which is the right place for it -- a
	// page size guessed into the manifest would be believed instead.
	t := &Template{
		SnapKey:     row.SnapKey,
		CreatedAt:   row.CreatedAt,
		PageSizeKiB: m.pageSizeKiB(),
	}
	if t.MemBuildID, err = uuid.Parse(row.MemBuildID); err != nil {
		return nil, fmt.Errorf("machines: fleet template has an unusable memory build %q: %w",
			row.MemBuildID, err)
	}
	if t.RootfsBuildID, err = uuid.Parse(row.RootfsBuildID); err != nil {
		return nil, fmt.Errorf("machines: fleet template has an unusable disk build %q: %w",
			row.RootfsBuildID, err)
	}

	start := time.Now()
	for _, id := range []uuid.UUID{t.MemBuildID, t.RootfsBuildID} {
		if err := m.materializeBuild(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := m.saveTemplate(t); err != nil {
		return nil, err
	}

	slog.Info("adopted the fleet's golden template",
		"seconds", int(time.Since(start).Seconds()),
		"mem_build", t.MemBuildID, "rootfs_build", t.RootfsBuildID)
	return t, nil
}

// materializeBuild pulls a build from object storage into the local build
// directory, unless it is already there.
//
// Chunkify reads a parent build straight off disk, with no storage
// credentials, so the template's builds have to exist as ordinary local build
// directories before this host can snapshot anything against them.
func (m *Manager) materializeBuild(ctx context.Context, id uuid.UUID) error {
	dir := filepath.Join(m.buildDir(), id.String())
	if _, err := os.Stat(filepath.Join(dir, "header")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
			return nil
		}
	}
	if m.opts.BlockStore == nil {
		return fmt.Errorf("machines: cannot pull build %s without object storage", id)
	}

	build, err := block.OpenRemoteBuild(ctx, m.opts.BlockStore, id, m.buildDir())
	if err != nil {
		return fmt.Errorf("machines: open template build %s: %w", id, err)
	}
	defer build.Close()

	// In full, not lazily: the point is to have the bytes on disk.
	if err := build.Prefault(ctx); err != nil {
		return fmt.Errorf("machines: pull template build %s: %w", id, err)
	}
	return nil
}

// loadTemplate reads a previously built template, checking that the builds it
// names are actually still on disk.
func (m *Manager) loadTemplate() (*Template, error) {
	raw, err := os.ReadFile(filepath.Join(m.templateRoot(), templateFile))
	if err != nil {
		return nil, err
	}
	var t Template
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, err
	}

	// A manifest naming builds that were cleaned away is worse than none: the
	// first create would fail on a missing header rather than rebuild.
	for _, dir := range []string{m.memParentDir(&t), m.rootfsTemplateDir(&t)} {
		if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
			return nil, fmt.Errorf("machines: template build %s is gone: %w", dir, err)
		}
	}

	// A memory image photographed at another page size cannot be restored
	// here at all -- Firecracker reads the size back out of the snapshot and
	// refuses to reinterpret it. Reporting "no template" rebuilds; returning
	// it would fail every create on this host instead, at restore time,
	// naming neither the page size nor the manifest.
	// No guard for a template with no memory build: the loop above already
	// refuses one, because memParentDir returns an empty path for it.
	if want := m.pageSizeKiB(); t.PageSizeKiB != want {
		return nil, fmt.Errorf("machines: template is %d KiB pages, this host "+
			"runs %d KiB: %w", t.PageSizeKiB, want, errTemplatePageSize)
	}
	return &t, nil
}

// errTemplatePageSize marks a template this host cannot restore because it was
// photographed at a different guest page size.
var errTemplatePageSize = errors.New("machines: template page size mismatch")

func (m *Manager) saveTemplate(t *Template) error {
	if err := os.MkdirAll(m.templateRoot(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: a truncated manifest read on the next start would look like a
	// corrupt template rather than an absent one.
	tmp := filepath.Join(m.templateRoot(), templateFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.templateRoot(), templateFile))
}

// buildTemplate boots one machine, lets it settle, and snapshots it.
func (m *Manager) buildTemplate(ctx context.Context) (*Template, error) {
	// A key of this build's own, not a constant.
	//
	// Two fresh hosts can each find no template and each build one. Under a
	// shared key the second upload overwrites the first, so the losing host's
	// published descriptor names a vmstate that now belongs to the winner --
	// its memory build paired with someone else's machine state. Whoever
	// restores against it gets a guest that was never captured.
	//
	// The wasted duplicate build is fine. Overwriting is not.
	t := &Template{
		SnapKey:     filepath.Join("template", uuid.NewString(), fc.SnapFile),
		CreatedAt:   time.Now().Unix(),
		PageSizeKiB: m.pageSizeKiB(),
	}

	// The disk template needs no VM at all: it is the golden rootfs, chunked.
	rootfsBuild := uuid.New()
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In:      m.opts.FCConfig.TemplateRootfs,
		OutDir:  filepath.Join(m.buildDir(), rootfsBuild.String()),
		BuildID: rootfsBuild,
	}); err != nil {
		return nil, fmt.Errorf("machines: chunkify the golden rootfs: %w", err)
	}
	if err := m.uploadBuild(ctx, rootfsBuild); err != nil {
		return nil, err
	}
	t.RootfsBuildID = rootfsBuild

	memBuild, err := m.captureTemplateMemory(ctx, t.SnapKey)
	if err != nil {
		return nil, err
	}
	t.MemBuildID = memBuild
	return t, nil
}

// captureTemplateMemory boots a throwaway machine and chunkifies its memory.
func (m *Manager) captureTemplateMemory(ctx context.Context, snapKey string) (uuid.UUID, error) {
	row := &state.Machine{
		// The prefix matters: it is how Manager.token knows this guest still
		// carries the golden rootfs's placeholder credential.
		ID: newID("tmpl"), VCPUs: 1, MemMiB: 512,
		AppPort: netns.GuestAppPort, AgentPort: netns.GuestAgentPort,
	}
	stateDir := m.stateDir(row.ID)
	defer os.RemoveAll(stateDir)

	slot, err := m.pool.Take(row.ID)
	if err != nil {
		return uuid.Nil, err
	}
	defer m.pool.Return(slot.Idx)

	mac, err := fc.GenerateMAC()
	if err != nil {
		return uuid.Nil, err
	}
	if err := netns.Setup(slot, mac, m.opts.FCConfig.JailUID); err != nil {
		return uuid.Nil, err
	}

	fcm, err := fc.Boot(ctx, m.machineFCConfig(row, slot, mac))
	if err != nil {
		_ = netns.Teardown(slot)
		return uuid.Nil, fmt.Errorf("machines: boot the template machine: %w", err)
	}
	defer func() { _ = fcm.Kill() }()

	if err := waitForAgent(ctx, slot.AgentAddr(), 60*time.Second); err != nil {
		return uuid.Nil, err
	}
	// Let systemd finish converging. Snapshotting a guest mid-transition
	// produces a template that restores into a machine which never finishes
	// starting.
	select {
	case <-time.After(settleTime):
	case <-ctx.Done():
		return uuid.Nil, ctx.Err()
	}

	// Flush before the capture, or the memory image holds writes the disk
	// template does not -- and every machine created from it inherits the
	// disagreement.
	m.execSync(ctx, row.ID, slot)

	snapshot, err := fcm.CaptureTemplate(ctx, fc.SnapshotOpts{BuildDir: m.buildDir()})
	if err != nil {
		return uuid.Nil, err
	}
	if err := m.uploadBuild(ctx, snapshot.MemBuildID); err != nil {
		return uuid.Nil, err
	}
	if err := m.opts.Uploader.PutFile(ctx, snapKey, snapshot.SnapPath); err != nil {
		return uuid.Nil, fmt.Errorf("machines: upload the template vmstate: %w", err)
	}
	return snapshot.MemBuildID, nil
}

// execSync flushes the guest's page cache. Best effort, like the suspend path:
// a slightly stale disk beats no template.
func (m *Manager) execSync(ctx context.Context, machineID string, slot *netns.Slot) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if _, err := m.execOnSlot(ctx, machineID, slot, api.ExecRequest{
		Cmd: "sync", User: "root", TimeoutMS: 10_000,
	}); err != nil {
		slog.Warn("could not flush the template machine's disk", "err", err)
	}
}

// uploadBuild publishes a build this host produced.
func (m *Manager) uploadBuild(ctx context.Context, id uuid.UUID) error {
	if m.opts.Chunks == nil {
		return errors.New("machines: no chunk store is configured")
	}
	return fc.UploadBuild(ctx, m.opts.Chunks, m.buildDir(), id)
}

// templateFor returns the template a machine's images are diffs against.
//
// NOT this host's current template. A machine's unchanged ranges resolve
// against its parent by offset, so attaching a different template hands the
// guest another machine's bytes -- and the two differ routinely: the golden
// template is rebuilt, or a host that has never held one mints its own with
// fresh ids. block.SetParent catches the mismatch and refuses, which turns
// silent corruption into a failed restore; this is what stops the restore
// failing in the first place.
//
// A machine created before the pin existed has no recorded template, and the
// host's own is the only answer available. That is the pre-existing behaviour,
// kept only for those rows.
func (m *Manager) templateFor(ctx context.Context, row *state.Machine) (*Template, error) {
	if row.TemplateMemBuildID == "" || row.TemplateRootfsBuildID == "" {
		return m.EnsureTemplate(ctx)
	}

	memID, err := uuid.Parse(row.TemplateMemBuildID)
	if err != nil {
		return nil, fmt.Errorf("machines: machine %s names an unusable template memory build (%q): %w",
			row.ID, row.TemplateMemBuildID, err)
	}
	rootfsID, err := uuid.Parse(row.TemplateRootfsBuildID)
	if err != nil {
		return nil, fmt.Errorf("machines: machine %s names an unusable template disk build (%q): %w",
			row.ID, row.TemplateRootfsBuildID, err)
	}

	// The host's own template, when it happens to be the same one. Saves
	// materialising what is already on disk, which is the common case.
	if t, err := m.loadTemplate(); err == nil &&
		t.MemBuildID == memID && t.RootfsBuildID == rootfsID {
		return t, nil
	}

	// A template this host has never held. The builds are content-addressed
	// and already in object storage, so this is a download rather than a
	// rebuild -- the same path adoptFleetTemplate takes, and what makes a
	// machine restorable on a host that has never seen its template.
	for _, id := range []uuid.UUID{memID, rootfsID} {
		// The nil memory build is the recorded absence of a parent, written by
		// a machine that booted. There is nothing to fetch.
		if id == uuid.Nil {
			continue
		}
		if err := m.materializeBuild(ctx, id); err != nil {
			return nil, fmt.Errorf("machines: fetch the template machine %s was built from: %w",
				row.ID, err)
		}
	}

	// The vmstate key is only needed to START from a template. A machine being
	// woken or restored has its own snapshot and never reads the template's.
	return &Template{MemBuildID: memID, RootfsBuildID: rootfsID}, nil
}

// validateMemMiB refuses a size this host cannot back.
//
// Firecracker rejects an odd mem_size_mib under 2MiB pages, and its own error
// names neither the field nor the reason -- so it arrives as a create failure
// that reads like a bug in hostd. Refusing at the API boundary puts the
// explanation where the caller can act on it.
func (m *Manager) validateMemMiB(memMiB int) error {
	if m.opts.FCConfig.HugePages && memMiB%2 != 0 {
		return fmt.Errorf("machines: mem_mib must be even on this host (%d is "+
			"odd); guest memory is backed by 2MiB pages", memMiB)
	}
	return nil
}
