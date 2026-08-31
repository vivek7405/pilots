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

	slog.Info("no golden template in the fleet; building one")
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
		ID: state.GoldenTemplate, MemBuildID: t.MemBuildID.String(),
		RootfsBuildID: t.RootfsBuildID.String(), SnapKey: t.SnapKey,
		CreatedAt: t.CreatedAt,
	}); err != nil {
		return nil, fmt.Errorf("machines: publish the golden template: %w", err)
	}

	if winner, err := m.adoptFleetTemplate(ctx); err == nil {
		if winner.MemBuildID != t.MemBuildID {
			slog.Info("another host published a golden template first; adopting it",
				"ours", t.MemBuildID, "theirs", winner.MemBuildID)
		}
		return winner, nil
	}

	if err := m.saveTemplate(t); err != nil {
		return nil, err
	}
	slog.Info("golden template ready", "seconds", int(time.Since(start).Seconds()),
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
	row, err := m.opts.Store.GetTemplate(ctx, state.GoldenTemplate)
	if err != nil {
		return nil, err
	}

	t := &Template{SnapKey: row.SnapKey, CreatedAt: row.CreatedAt}
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
	return &t, nil
}

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
	t := &Template{SnapKey: filepath.Join("template", fc.SnapFile), CreatedAt: time.Now().Unix()}

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
