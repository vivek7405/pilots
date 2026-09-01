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
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vivek7405/pilots/hostd/internal/api"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/nbd"
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

	// Chunks writes content-addressed builds, and BlockStore reads them back.
	// Both address the same objects under the chunk prefix; they are separate
	// fields only because uploading and lazy reading have different shapes.
	Chunks     fc.Uploader
	BlockStore block.ObjectStore

	// NBDDevices hands out the kernel block devices machines' disks are
	// served on. One pool per host, because the devices are a host resource.
	NBDDevices *nbd.DevicePool

	// HandlerEnv is passed to the block and fault servers, which need the
	// object-storage credentials to read builds.
	HandlerEnv []string

	// AgentTokenSecret derives each machine's guest credential. See
	// Manager.token for why a rescued machine is unreachable without it.
	AgentTokenSecret string

	// Volumes creates and mounts persistent disks. Nil on a host with no
	// object storage, where every volume operation is refused up front rather
	// than failing somewhere inside a create.
	Volumes VolumeManager
	// MachinePrefix is this host's block of mesh addresses, derived from its
	// WireGuard key. Every slot's address comes out of it, so a host without
	// one runs machines that cannot reach a peer -- correct on a single box,
	// where there is no peer to reach.
	MachinePrefix netip.Prefix
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
		pool:    netns.NewPool(opts.PoolSize, opts.MachinePrefix),
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

// Create brings up a new machine by restoring the golden template.
//
// Restoring, not booting: the template is a machine that already finished
// booting and settling, so a create costs a snapshot load rather than twenty
// seconds of kernel and systemd.
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
	// Derived rather than random when the fleet has a secret, so that any host
	// can reach this machine after rescuing it. See Manager.token.
	token := newID("agt")
	if m.opts.AgentTokenSecret != "" {
		token = m.token(id)
	}
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

	fcm, err := m.startNewMachine(ctx, row, token, req.Volume, req.Image)
	if err != nil {
		row.State = StateError
		stampSlot(row, nil)
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		return row, err
	}

	m.put(id, fcm)
	m.rememberToken(id, token)

	row.State = StateRunning
	stampSlot(row, fcm)
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return row, err
	}
	return row, nil
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

	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		return err
	}

	var errs []error

	if fcm, ok := m.get(id); ok {
		// The copy-on-write file holds every write since the last snapshot.
		// Destroy is the ONLY point at which discarding it is correct.
		defer fcm.DiscardCow()
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

	// After the process is gone, never before: unmounting a volume under a
	// live guest loses its last writes silently rather than failing.
	if err := m.releaseVolume(ctx, row.VolumeID); err != nil {
		errs = append(errs, fmt.Errorf("release volume: %w", err))
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

	keys := []string{suspendSnapKey(id), prefetchKey(id)}

	// The machine's content-addressed builds go too. Each build id belongs to
	// exactly one snapshot of one machine, so nothing else reads them -- and
	// they are the large objects. The template's builds are shared and are
	// deliberately not touched here.
	var builds []string
	if row, err := m.opts.Store.GetMachine(ctx, id); err == nil {
		builds = append(builds, row.MemBuildID, row.RootfsBuildID)
	}
	if cks, err := m.opts.Store.ListCheckpoints(ctx, id); err == nil {
		for _, c := range cks {
			keys = append(keys, checkpointSnapKey(id, c.ID))
			builds = append(builds, c.MemBuildID, c.RootfsBuildID)
		}
	}

	var errs []error
	for _, key := range keys {
		if err := deleter.Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", key, err))
		}
	}
	m.discardBuilds(ctx, builds...)
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

	// The template this machine was built from, not this host's current one.
	// A suspend writes a DIFF, and its base is recorded in the header, so
	// capturing against the wrong template produces an image that no host --
	// including this one -- can ever resolve correctly again.
	t, err := m.templateFor(ctx, row)
	if err != nil {
		return err
	}

	res, err := fcm.SuspendInstant(ctx, m.opts.Uploader, m.opts.Chunks, fc.SnapshotOpts{
		MemParentDir:      m.memParentDir(t),
		RootfsTemplateDir: m.rootfsTemplateDir(t),
		BuildDir:          m.buildDir(),
	}, suspendSnapKey(id), prefetchKey(id))
	if err != nil {
		return err
	}
	m.drop(id)
	if slotIdx > 0 {
		m.pool.Return(slotIdx)
	}

	// The builds this suspend replaces. Nothing else can be reading them: a
	// checkpoint mints its own ids, so a machine's suspend builds are named by
	// its row and nowhere else.
	superseded := []string{row.MemBuildID, row.RootfsBuildID}

	row.State = StateSuspended
	// The slot is gone: a suspended machine holds no index and is addressable
	// nowhere until it wakes, possibly on another host and certainly in
	// another slot.
	stampSlot(row, nil)
	row.MemBuildID = res.MemBuildID.String()
	// An empty rootfs build is meaningful: the machine wrote nothing, so its
	// next restore reads the template directly. Writing a zero uuid instead
	// would send the wake looking for a build that was never created.
	row.RootfsBuildID = ""
	if res.RootfsBuildID != uuid.Nil {
		row.RootfsBuildID = res.RootfsBuildID.String()
	}
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return err
	}

	// Only AFTER the row names the new builds. Deleting first would, on a
	// failed write, leave the row pointing at objects that no longer exist --
	// a machine that cannot be woken anywhere.
	m.discardBuilds(ctx, superseded...)
	return nil
}

// discardBuilds removes builds nothing references any more.
//
// Every suspend writes a fresh memory build, so without this each one leaks a
// full memory diff into object storage forever -- the largest objects the
// system produces, growing without bound for a machine that wakes and sleeps
// on a schedule.
//
// Best effort: a failure here costs storage, while failing the suspend over it
// would cost the machine.
func (m *Manager) discardBuilds(ctx context.Context, ids ...string) {
	deleter, ok := m.opts.Chunks.(interface {
		Delete(ctx context.Context, key string) error
	})
	if !ok {
		return
	}

	for _, id := range ids {
		if id == "" {
			continue
		}
		for _, name := range []string{id + "/header", id + "/data"} {
			if err := deleter.Delete(ctx, name); err != nil {
				slog.Warn("a superseded build was left in object storage",
					"build", id, "key", name, "err", err)
			}
		}
	}
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

	fcm, err := m.wakeFromSuspend(ctx, row)
	if err != nil {
		row.State = StateError
		stampSlot(row, nil)
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		return err
	}
	m.put(id, fcm)

	row.State = StateRunning
	stampSlot(row, fcm)
	row.LastActivity = time.Now().Unix()
	row.UpdatedAt = time.Now().Unix()
	return m.opts.Store.PutMachine(ctx, row)
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

	// The volume image, for every path that brings this machine up: a boot
	// declares the drive from it, and a restore has the drive already in its
	// snapshot and needs a file at the baked path for it to resolve against.
	// One place, so neither can be forgotten.
	if row.VolumeID != "" && m.opts.Volumes != nil {
		cfg.VolumeImage = m.opts.Volumes.ImagePath(row.VolumeID)
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
