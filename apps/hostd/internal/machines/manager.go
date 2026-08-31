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
	return fc.Artifacts{Prefix: filepath.Join("machines", machineID, "checkpoints", checkpointID)}
}

// localCacheDir mirrors the object-storage layout on disk.
//
// This MUST be per-artifact-set, not merely per-machine. Restores fetch only
// what is missing locally, so if a machine's suspend image and its checkpoints
// shared one directory, a file left by an earlier restore would shadow the one
// being fetched -- and the machine would silently come back with the wrong
// disk. Mirroring the remote layout makes that collision impossible.
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

	// The agent token is generated once and only its hash is stored. hostd
	// keeps the plaintext in memory for as long as it drives this machine; a
	// restart re-mints it, since nothing else needs to reproduce it.
	token := newID("agt")
	sum := sha256.Sum256([]byte(token))

	knobs := api.Knobs{AutoStop: "suspend", AutoStart: true, SoftLimit: 20}
	if req.Knobs != nil {
		knobs = *req.Knobs
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

	cfg := m.opts.FCConfig
	cfg.MachineID = row.ID
	cfg.Slot = slot
	cfg.MAC = mac
	cfg.VCPUs = row.VCPUs
	cfg.MemMiB = row.MemMiB
	cfg.StateDir = m.stateDir(row.ID)

	fcm, err := fc.Boot(ctx, cfg)
	if err != nil {
		_ = netns.Teardown(slot)
		m.pool.Return(slot.Idx)
		return nil, err
	}

	// Give the guest its own token, replacing the template's placeholder.
	if err := m.installToken(ctx, slot, token); err != nil {
		slog.Warn("could not install the machine's agent token", "machine", row.ID, "err", err)
	}

	if err := fcm.Persist(); err != nil {
		// Not fatal: the machine is running and serving. But a restart will
		// not re-adopt it, which is worth shouting about.
		slog.Error("machine is running but its breadcrumbs were not written; "+
			"a hostd restart will not re-adopt it", "machine", row.ID, "err", err)
	}
	return fcm, nil
}

// Destroy stops a machine and removes every trace of it.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return err
	}

	if fcm, ok := m.get(id); ok {
		slotIdx := 0
		if fcm.Slot != nil {
			slotIdx = fcm.Slot.Idx
		}
		if err := fcm.Kill(); err != nil {
			return err
		}
		m.drop(id)
		if slotIdx > 0 {
			m.pool.Return(slotIdx)
		}
	}

	_ = os.RemoveAll(m.stateDir(id))
	_ = os.RemoveAll(m.cacheDir(id))
	m.forgetToken(id)

	_ = row // row is fetched to confirm existence before destroying
	return m.opts.Store.DeleteMachine(ctx, id)
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

	cfg := m.opts.FCConfig
	cfg.MachineID = row.ID
	cfg.Slot = slot
	cfg.MAC = mac
	cfg.VCPUs = row.VCPUs
	cfg.MemMiB = row.MemMiB
	cfg.StateDir = m.stateDir(row.ID)

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
func (m *Manager) Adopt(id string, fcm *fc.Machine, slotIdx int) error {
	if slotIdx > 0 {
		if _, err := m.pool.Reserve(slotIdx, id); err != nil {
			return err
		}
	}
	m.put(id, fcm)
	return nil
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
