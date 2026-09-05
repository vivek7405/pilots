package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The vendor decision, in one place.
//
// A Firecracker memory snapshot carries raw CPUID and never restores across
// the Intel/AMD boundary, so a machine whose owner is gone and whose pool has
// no live host cannot be resumed anywhere. The disk half is vendor-free: the
// rootfs is an ext4 image with no CPUID in it, and reclaimChain runs `sync`
// inside the guest before every snapshot, so the disk captured at suspend is
// filesystem-consistent rather than crash-consistent. Booting it is a clean
// boot, not a journal recovery.
//
// So the machine cold-boots: same id, same name, same URL, same volume, same
// agent token, same bytes on disk, and none of its processes or memory. That
// is worse than a resume and far better than staying down, and it is automatic
// with no per-machine knob -- availability wins over continuity, uniformly.

// noApplicationCommand is what the guest agent says when there is nothing to
// start. A bare sandbox answers this on every cold boot and it is not a fault;
// kept in step with cmd/guest-agent/init.go by TestAColdBootIsQuietAboutABareSandbox.
const noApplicationCommand = "this image carries no application command"

// bringUp starts a machine's memory image here by whichever path this CPU
// allows, and says which it took. The ONE place the vendor decision is made.
func (m *Manager) bringUp(ctx context.Context, row *state.Machine) (*fc.Machine, string, error) {
	if row.MemBuildID == "" {
		// Never snapshotted, so there is no disk in object storage to boot
		// from either. The same message wakeFromSuspend produces, because the
		// caller's handling is the same.
		return nil, "", fmt.Errorf("machines: machine %s has no usable memory build (%q)",
			row.ID, row.MemBuildID)
	}

	cpu, err := m.opts.Store.GetMachineCPU(ctx, row.ID)
	if err == nil && cpu.Vendor != "" && cpu.Vendor != m.opts.Vendor {
		slog.Warn("cold-booting a machine from its disk: its memory image belongs to another CPU vendor",
			"machine", row.ID, "image_vendor", cpu.Vendor, "host_vendor", m.opts.Vendor)
		fcm, err := m.bootFromDisk(ctx, row, fc.Backends{}, row.RootfsBuildID)
		return fcm, state.StartColdBoot, err
	}

	fcm, err := m.wakeFromSuspend(ctx, row)
	return fcm, state.StartRestore, err
}

// bootFromDisk boots a machine's own disk chain with a kernel, discarding the
// memory image it cannot load.
//
// It mirrors wakeFromSuspend minus the memory, and restore's tail minus the
// vmstate. It is deliberately NOT bootMachine: that boots a plain rootfs FILE
// reflink-copied from a template, which would discard every write this machine
// ever made.
func (m *Manager) bootFromDisk(ctx context.Context, row *state.Machine,
	backends fc.Backends, rootfsBuildID string) (*fc.Machine, error) {

	// Refused rather than defaulted. templateFor falls back to EnsureTemplate
	// when the row names no template, and EnsureTemplate returns THIS host's
	// pool template -- whose rootfs build id differs from the other pool's even
	// though the bytes are identical, because each pool's first host
	// chunkified its own copy. Resolving this machine's disk diff against the
	// wrong parent is silent corruption, so a row in that state does not boot.
	// No machine created by current code is in it.
	if row.TemplateRootfsBuildID == "" {
		return nil, fmt.Errorf("machines: machine %s names no template disk build, so its "+
			"disk diff has no parent to resolve against on this host", row.ID)
	}
	t, err := m.templateFor(ctx, row)
	if err != nil {
		return nil, err
	}

	backends.RootfsTemplateDir = m.rootfsTemplateDir(t)
	backends.CacheRoot = m.buildDir()
	// An absent rootfs build is normal and means the machine wrote nothing to
	// disk and reads the template directly, exactly as on a wake.
	if rootfsBuildID != "" {
		if backends.RootfsDiffID, err = uuid.Parse(rootfsBuildID); err != nil {
			return nil, fmt.Errorf("machines: machine %s has an unusable disk build (%q): %w",
				row.ID, rootfsBuildID, err)
		}
	}

	// The SAME machine id, always. claimVolume refuses a volume held by a
	// different machine, and re-claiming for this one is a no-op by
	// construction; minting a new id here is the one way this could break.
	var vol *state.Volume
	if row.VolumeID != "" {
		v, err := m.claimVolume(ctx, row.VolumeID, row.ID)
		if err != nil {
			return nil, fmt.Errorf("machines: attach volume %s for %s: %w",
				row.VolumeID, row.ID, err)
		}
		vol = v
	}

	// takeSlot, never pool.Take: a cold boot on the OWNING host reuses the
	// index the replica kept while it slept, so its mesh address does not move.
	// A rescue takes a fresh one, because Rescue has already written the row's
	// slot to zero.
	slot, err := m.takeSlot(row)
	if err != nil {
		return nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}

	cfg := m.machineFCConfig(row, slot, mac)
	if row.ImageRef != "" {
		// The kernel is told what to run, exactly as bootMachine tells it: an
		// image built from a Dockerfile ships no /sbin/init of ours.
		cfg.InitPath = build.AgentPathInImage
	}

	fcm, err := fc.BootFromDisk(ctx, fc.InstantConfig{
		Config:   cfg,
		Backends: backends,
		Env:      m.opts.HandlerEnv,
	}, m.opts.BlockStore, m.opts.NBDDevices)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("machines: cold boot %s: %w", row.ID, err)
	}

	m.bindDiscovery(row.ID, slot)

	// No installToken. The credential is already at /etc/pilot-agent/token on
	// the machine's own disk, written at create and synced by reclaimChain
	// before the suspend, and the agent reads it at start.
	if err := waitForAgent(ctx, slot.AgentAddr(), 90*time.Second); err != nil {
		m.releaseDiscovery(row.ID)
		_ = fcm.Kill()
		m.pool.Return(slot.Idx)
		return nil, fmt.Errorf("machines: cold boot %s: agent never answered: %w", row.ID, err)
	}
	if vol != nil {
		if err := m.mountVolumeInGuest(ctx, slot, row.ID, m.token(row.ID), vol.MountPath); err != nil {
			m.releaseDiscovery(row.ID)
			_ = fcm.Kill()
			m.pool.Return(slot.Idx)
			return nil, err
		}
	}
	m.restartApp(ctx, row, slot)

	if err := fcm.Persist(); err != nil {
		// Not fatal: the machine is running and serving. But a restart will
		// not re-adopt it, which is worth shouting about.
		slog.Error("cold-booted machine's breadcrumbs were not written",
			"machine", row.ID, "err", err)
	}

	// No memory parent, recorded explicitly rather than left empty, for the
	// reason bootMachine gives: this guest booted, so its pages are no
	// template's diff. The DISK pins are untouched -- the chain this machine
	// just booted from is the chain its next suspend diffs against.
	row.TemplateMemBuildID = uuid.Nil.String()
	return fcm, nil
}

// restartApp asks a cold-booted guest's agent to start its application.
//
// A kernel boot leaves pilot-app.service stopped: the unit is started by the
// guest agent and is never enabled in the image. The poke carries NEITHER an
// environment NOR a command, which is what marks it a restart rather than a
// create: everything the application needs is already in /etc/pilot on the
// machine's own disk, and rewriting it from the build's start spec would drop
// a Dockerfile image's deploy-time environment.
//
// A separate function from deliverEnv on purpose, so that the create path
// keeps exactly one caller list and there is no flag to get wrong.
func (m *Manager) restartApp(ctx context.Context, row *state.Machine, slot *netns.Slot) {
	body, err := json.Marshal(initPayload{
		TimestampNanos: time.Now().UnixNano(),
		StartApp:       true,
	})
	if err != nil {
		slog.Error("could not encode the restart poke", "machine", row.ID, "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/init", bytes.NewReader(body))
	if err != nil {
		slog.Error("could not build the restart poke", "machine", row.ID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token(row.ID))

	resp, err := (&http.Client{Timeout: initTimeout}).Do(req)
	if err != nil {
		slog.Error("a cold-booted machine's application was not started",
			"machine", row.ID, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Error("a cold-booted machine's application was not started",
			"machine", row.ID, "status", resp.StatusCode)
		return
	}

	var out initResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		slog.Error("could not read the restart response", "machine", row.ID, "err", err)
		return
	}
	if out.AppStarted || out.AppReason == "" {
		return
	}
	if out.AppReason == noApplicationCommand {
		// A bare sandbox has no application to start, which is not a fault.
		slog.Debug("a cold-booted sandbox has no application to start", "machine", row.ID)
		return
	}
	slog.Warn("a cold-booted machine's application was not started",
		"machine", row.ID, "reason", out.AppReason)
}

// recordStart writes which vendor brought a machine up and how, and counts it.
//
// It makes NO usage-ledger call: the ledger is keyed on state, and a cold boot
// ends in StateRunning exactly as a restore does. On a cold boot it also clears
// the memory build, because the guest booted and its disk has moved on: the
// image is wrong everywhere now, and resuming it later on a same-vendor host
// would pair stale memory with a newer disk -- the disagreement `sync` before
// every snapshot exists to prevent.
//
// The caller writes the row. The returned function discards the superseded
// objects and must run AFTER that write, so nothing can read a row naming a
// build that is already gone.
func (m *Manager) recordStart(ctx context.Context, row *state.Machine, kind string,
	opts ...state.WriteOption) func() {

	metrics.MachineStarts.With(kind).Inc()

	now := time.Now().Unix()
	if err := m.opts.Store.PutMachineCPU(ctx, &state.MachineCPU{
		ID: row.ID, Kind: state.KindMachine, Vendor: m.opts.Vendor,
		LastStart: kind, LastStartAt: now, UpdatedAt: now,
	}, opts...); err != nil {
		// Not fatal: the machine is up. But every peer now computes this
		// machine's rescue tier from a stale pool, so it is worth shouting.
		slog.Error("a machine's start was not recorded; peers will rank its "+
			"rescue from a stale CPU vendor", "machine", row.ID, "err", err)
	}

	if kind != state.StartColdBoot {
		return func() {}
	}
	superseded := row.MemBuildID
	row.MemBuildID = ""
	return func() {
		m.discardBuilds(ctx, superseded)
		deleter, ok := m.opts.Uploader.(interface {
			Delete(ctx context.Context, key string) error
		})
		if !ok {
			return
		}
		for _, key := range []string{suspendSnapKey(row.ID), prefetchKey(row.ID)} {
			if err := deleter.Delete(ctx, key); err != nil {
				slog.Warn("a superseded suspend image was left in object storage",
					"machine", row.ID, "key", key, "err", err)
			}
		}
	}
}
