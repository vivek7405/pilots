// Package machines owns machine lifecycle: create, destroy, suspend, wake,
// checkpoint, and restore.
//
// One primitive. A sandbox and a production service are the same machine with
// different lifecycle knobs, so there is exactly one implementation of each
// verb here and no branching on "kind".
package machines

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Machine states.
const (
	StateCreating  = "creating"
	StateRunning   = "running"
	StateSuspended = "suspended"
	StateStopped   = "stopped"
	StateError     = "error"
)

// ErrNotFound wraps the store's sentinel so errors.Is works across the
// package boundary and the API layer can map it to a 404 without importing
// this package.
var ErrNotFound = fmt.Errorf("machines: %w", state.ErrNotFound)

// Options configures the manager.
type Options struct {
	HostID    string
	Domain    string // e.g. "pilotrun.app"
	StateRoot string // /var/lib/pilots/machines
	CacheRoot string // /var/cache/pilots
	FCConfig  fc.Config
	Store     state.Store
	Uploader  fc.Uploader
	PoolSize  int
}

// Manager is the per-host machine registry.
//
// Locking is per-machine, not global: suspending one machine must not block
// creating another, and wake-on-request is on the hot path for every suspended
// machine's first request.
type Manager struct {
	opts Options
	pool *netns.Pool

	mu      sync.RWMutex
	running map[string]*fc.Machine // machine id -> live process

	locks  sync.Map // machine id -> *sync.Mutex
	flight *inFlight
}

func New(opts Options) *Manager {
	if opts.PoolSize == 0 {
		opts.PoolSize = netns.DefaultPoolSize
	}
	return &Manager{
		opts:    opts,
		pool:    netns.NewPool(opts.PoolSize),
		running: make(map[string]*fc.Machine),
		flight:  newInFlight(),
	}
}

// lockFor serialises operations on one machine. Without it, a wake racing the
// idle monitor's suspend can leave a half-restored machine.
func (m *Manager) lockFor(id string) *sync.Mutex {
	l, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	return l.(*sync.Mutex)
}

func (m *Manager) get(id string) (*fc.Machine, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fcm, ok := m.running[id]
	return fcm, ok
}

func (m *Manager) put(id string, fcm *fc.Machine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running[id] = fcm
}

func (m *Manager) drop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, id)
}

// GCOrphanInterfaces removes veth links belonging to no live machine.
//
// A veth normally dies with its namespace, but an interrupted teardown can
// strand one, and they accumulate silently across crashes. Called on startup,
// once the machines that survived have been adopted -- anything not claimed by
// then has no owner.
func (m *Manager) GCOrphanInterfaces() error {
	m.mu.RLock()
	inUse := make(map[string]bool, len(m.running))
	for _, fcm := range m.running {
		if fcm.Slot != nil {
			inUse[fcm.Slot.VEthName] = true
		}
	}
	m.mu.RUnlock()
	return netns.GCOrphanVeths(inUse)
}

// Running reports the machines this host currently has processes for.
func (m *Manager) Running() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.running))
	for id := range m.running {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) stateDir(id string) string { return filepath.Join(m.opts.StateRoot, id) }
func (m *Manager) cacheDir(id string) string { return filepath.Join(m.opts.CacheRoot, id) }

// artifactsFor returns the object-storage prefix for a machine's suspend
// image. A checkpoint gets its own prefix so it is never overwritten by a
// later suspend.
func artifactsFor(machineID string) fc.Artifacts {
	return fc.Artifacts{Prefix: filepath.Join("machines", machineID, "suspend")}
}

func checkpointArtifacts(machineID, checkpointID string) fc.Artifacts {
	return fc.Artifacts{
		Prefix: filepath.Join("machines", machineID, "checkpoints", checkpointID),
		// A checkpoint is written once under its own id and never rewritten,
		// so a local copy is always current and can be reused. A suspend image
		// is the opposite: one prefix, overwritten every time.
		Immutable: true,
	}
}

// localCacheDir mirrors the object-storage layout on disk.
//
// Per-artifact-set, not merely per-machine: sharing one directory between a
// machine's suspend image and its checkpoints let a file from one restore
// shadow the other. Note this alone is not sufficient -- within the suspend
// set the prefix is reused on every suspend, so Restore additionally
// re-fetches mutable sets rather than trusting what is on disk.
func (m *Manager) localCacheDir(machineID string, at fc.Artifacts) string {
	return filepath.Join(m.opts.CacheRoot, at.Prefix)
}

// Create boots a new machine from the golden template.
func (m *Manager) Create(ctx context.Context, req api.CreateMachineRequest) (*state.Machine, error) {
	id := newID("m")

	name := req.Name
	if name == "" {
		name = generateName()
	}
	// A name becomes a DNS label and a routing key, so it has to be usable as
	// both before the machine exists.
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := m.ensureNameFree(ctx, name); err != nil {
		return nil, err
	}

	// The agent token is generated once and only its hash is stored. hostd
	// keeps the plaintext in memory for as long as it drives this machine; a
	// restart re-mints it, since nothing else needs to reproduce it.
	token := newID("agt")
	sum := sha256.Sum256([]byte(token))

	// A partial knobs object merges onto the defaults rather than replacing
	// them; see api.DecodeKnobs for why that distinction matters.
	knobs, err := api.DecodeKnobs(req.Knobs)
	if err != nil {
		return nil, err
	}
	knobsJSON, err := marshalKnobs(knobs)
	if err != nil {
		return nil, err
	}

	row := &state.Machine{
		ID: id, Name: name, HostID: m.opts.HostID, State: StateCreating,
		KindKnobs: knobsJSON,
		VCPUs:     orDefault(req.VCPUs, 1),
		MemMiB:    orDefault(req.MemMiB, 512),
		Domain:    name + "." + m.opts.Domain,
		AppPort:   netns.GuestAppPort, AgentPort: netns.GuestAgentPort,
		AgentTokenHash: hex.EncodeToString(sum[:]),
		LastActivity:   time.Now().Unix(),
		UpdatedAt:      time.Now().Unix(),
	}
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return nil, err
	}

	fcm, err := m.boot(ctx, row, token)
	if err != nil {
		row.State = StateError
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		return row, err
	}

	m.put(id, fcm)
	m.rememberToken(id, token)

	row.State = StateRunning
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return row, err
	}
	return row, nil
}

func (m *Manager) boot(ctx context.Context, row *state.Machine, token string) (*fc.Machine, error) {
	slot, err := m.pool.Take(row.ID)
	if err != nil {
		return nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}
	if err := netns.Setup(slot, mac, m.opts.FCConfig.JailUID); err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}

	cfg := m.machineFCConfig(row, slot, mac)

	fcm, err := fc.Boot(ctx, cfg)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, err
	}

	// Give the guest its own token, replacing the template's placeholder.
	//
	// Fatal, not a warning. hostd stores the new token while the guest keeps
	// the shared placeholder, and token() falls back to that same placeholder
	// -- so exec, the clock poke and the flush all keep working and the machine
	// is indistinguishable from a healthy one, while actually running on a
	// credential every other machine from this template also has.
	if err := m.installToken(ctx, slot, token); err != nil {
		return nil, fmt.Errorf("install agent token: %w", err)
	}

	if err := fcm.Persist(); err != nil {
		// Not fatal: the machine is running and serving. But a restart will
		// not re-adopt it, which is worth shouting about.
		slog.Error("machine is running but its breadcrumbs were not written; "+
			"a hostd restart will not re-adopt it", "machine", row.ID, "err", err)
	}
	return fcm, nil
}

// Destroy stops a machine and removes its state from the host AND from object
// storage.
//
// Every step runs even if an earlier one fails, and the errors are reported
// together. Abandoning cleanup on the first failure left the worst possible
// shape: the process gone but the slot still held, the caches still on disk,
// the row still in the store -- and Kill aggregates its own errors, so a single
// stubborn namespace could strand everything else.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if _, err := m.opts.Store.GetMachine(ctx, id); err != nil {
		return err
	}

	var errs []error

	if fcm, ok := m.get(id); ok {
		slotIdx := 0
		if fcm.Slot != nil {
			slotIdx = fcm.Slot.Idx
		}
		if err := fcm.Kill(); err != nil {
			errs = append(errs, fmt.Errorf("kill: %w", err))
		}
		// Release the slot and the registry entry regardless: the process is
		// gone or unreachable either way, and holding them leaks a slot per
		// failed destroy until the pool is exhausted.
		m.drop(id)
		if slotIdx > 0 {
			m.pool.Return(slotIdx)
		}
	}

	if err := os.RemoveAll(m.stateDir(id)); err != nil {
		errs = append(errs, fmt.Errorf("remove state dir: %w", err))
	}
	// The machine's caches live under the object-storage layout, not under a
	// bare id -- removing the wrong path meant every destroyed machine left its
	// full memory and disk images behind forever.
	if err := os.RemoveAll(filepath.Join(m.opts.CacheRoot, "machines", id)); err != nil {
		errs = append(errs, fmt.Errorf("remove cache: %w", err))
	}
	m.forgetToken(id)

	if err := m.deleteRemoteState(ctx, id); err != nil {
		errs = append(errs, err)
	}
	if err := m.deleteCheckpointRows(ctx, id); err != nil {
		errs = append(errs, err)
	}
	if err := m.opts.Store.DeleteMachine(ctx, id); err != nil {
		errs = append(errs, fmt.Errorf("delete row: %w", err))
	}
	return errors.Join(errs...)
}

// deleteRemoteState removes a machine's objects.
//
// Object storage is the only truth for machine state, so state left there is a
// machine that still exists in every way that matters -- and it is billed for
// and readable indefinitely.
func (m *Manager) deleteRemoteState(ctx context.Context, id string) error {
	deleter, ok := m.opts.Uploader.(interface {
		Delete(ctx context.Context, key string) error
	})
	if !ok {
		return nil // no object storage configured
	}

	sets := []fc.Artifacts{artifactsFor(id)}
	if cks, err := m.opts.Store.ListCheckpoints(ctx, id); err == nil {
		for _, c := range cks {
			sets = append(sets, checkpointArtifacts(id, c.ID))
		}
	}

	var errs []error
	for _, at := range sets {
		for _, key := range []string{at.Snap(), at.Mem(), at.Rootfs()} {
			if err := deleter.Delete(ctx, key); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", key, err))
			}
		}
	}
	return errors.Join(errs...)
}

// deleteCheckpointRows removes the machine's checkpoints, which would
// otherwise outlive it as rows pointing at objects that no longer exist.
func (m *Manager) deleteCheckpointRows(ctx context.Context, id string) error {
	cks, err := m.opts.Store.ListCheckpoints(ctx, id)
	if err != nil {
		return fmt.Errorf("list checkpoints: %w", err)
	}
	var errs []error
	for _, c := range cks {
		if err := m.opts.Store.DeleteCheckpoint(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("delete checkpoint %s: %w", c.ID, err))
		}
	}
	return errors.Join(errs...)
}

// Suspend snapshots a machine to object storage and stops it.
//
// The machine keeps its identity: same row, same URL, same token. Only the
// process and its host resources go away.
func (m *Manager) Suspend(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return err
	}
	fcm, ok := m.get(id)
	if !ok {
		// Already suspended or stopped: nothing to do.
		return nil
	}

	slotIdx := 0
	if fcm.Slot != nil {
		slotIdx = fcm.Slot.Idx
	}

	// The guest must write out its page cache before we capture the disk, or
	// the memory and disk images disagree about recent writes.
	m.flushGuestDisk(ctx, id)

	if _, err := fcm.Suspend(ctx, m.opts.Uploader, artifactsFor(id)); err != nil {
		return err
	}
	m.drop(id)
	if slotIdx > 0 {
		m.pool.Return(slotIdx)
	}

	row.State = StateSuspended
	row.UpdatedAt = time.Now().Unix()
	return m.opts.Store.PutMachine(ctx, row)
}

// Wake restores a suspended machine.
//
// Idempotent and safe to call concurrently: the per-machine lock means a burst
// of requests to a sleeping machine produces exactly one restore, and the rest
// wait for it.
func (m *Manager) Wake(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if _, ok := m.get(id); ok {
		return nil // another waker got there first
	}

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return err
	}

	fcm, err := m.restore(ctx, row, artifactsFor(id))
	if err != nil {
		row.State = StateError
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		return err
	}
	m.put(id, fcm)

	row.State = StateRunning
	row.LastActivity = time.Now().Unix()
	row.UpdatedAt = time.Now().Unix()
	return m.opts.Store.PutMachine(ctx, row)
}

// restore rebuilds a machine from artifacts. Shared by wake and
// checkpoint-restore, because they are the same operation.
func (m *Manager) restore(ctx context.Context, row *state.Machine, at fc.Artifacts) (*fc.Machine, error) {
	slot, err := m.pool.Take(row.ID)
	if err != nil {
		return nil, err
	}
	mac, err := fc.GenerateMAC()
	if err != nil {
		m.pool.Return(slot.Idx)
		return nil, err
	}

	cfg := m.machineFCConfig(row, slot, mac)

	fcm, err := fc.Restore(ctx, fc.RestoreConfig{
		Config:     cfg,
		Artifacts:  at,
		LocalDir:   m.localCacheDir(row.ID, at),
		AgentToken: m.token(row.ID),
	}, m.opts.Uploader)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, err
	}

	if err := fcm.Persist(); err != nil {
		slog.Error("restored machine's breadcrumbs were not written",
			"machine", row.ID, "err", err)
	}
	return fcm, nil
}

// Adopt re-registers a machine that survived a hostd restart.
//
// Reserving the slot does two things at once: it stops the pool handing that
// index to a new machine, and it rebuilds the Slot itself -- which the adopted
// machine needs, because the router and every exec reach a guest through its
// slot address. Without reattaching it the machine would be running and
// routable in the registry but unreachable in practice.
func (m *Manager) Adopt(id string, fcm *fc.Machine, slotIdx int) error {
	if slotIdx <= 0 {
		m.put(id, fcm)
		return nil
	}
	slot, err := m.pool.Reserve(slotIdx, id)
	if err != nil {
		return err
	}
	fcm.Slot = slot
	m.put(id, fcm)
	return nil
}

// machineFCConfig derives a machine's Firecracker configuration, including the
// cgroup limits that bound what a guest can take from its neighbours.
//
// Firecracker isolates the guest from the host kernel, but nothing stops it
// consuming every core or all remaining memory -- including hostd's own. The
// limits were previously documented as mandatory and then never set from the
// machine's shape, so only a pid cap was ever applied.
func (m *Manager) machineFCConfig(row *state.Machine, slot *netns.Slot, mac string) fc.Config {
	cfg := m.opts.FCConfig
	cfg.MachineID = row.ID
	cfg.Slot = slot
	cfg.MAC = mac
	cfg.VCPUs = row.VCPUs
	cfg.MemMiB = row.MemMiB
	cfg.StateDir = m.stateDir(row.ID)

	// Memory: the guest's own size plus a margin for Firecracker's own
	// allocations, so the VMM is not OOM-killed for doing its job.
	const vmmOverheadMiB = 128
	cfg.Limits.MemMaxB = int64(row.MemMiB+vmmOverheadMiB) * 1024 * 1024

	// CPU: vcpus worth of a 100ms period, so a machine cannot exceed the cores
	// it was sold.
	const cpuPeriodUS = 100_000
	cfg.Limits.CPUMax = fmt.Sprintf("%d %d", row.VCPUs*cpuPeriodUS, cpuPeriodUS)

	if cfg.Limits.PidsMax == 0 {
		cfg.Limits.PidsMax = 2048
	}
	return cfg
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// newID mints an identifier.
//
// Hyphen-separated, not underscore: a machine id becomes the jailer's instance
// id, and the jailer accepts only alphanumerics and hyphens. Sanitising at
// every use would be one more thing to forget, so the ids are simply safe
// everywhere they appear -- jail, URL, and API alike.
func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("machines: entropy unavailable: %v", err))
	}
	return prefix + "-" + hex.EncodeToString(b)
}
