package machines

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// RunRootFlush makes every running machine's disk durable on a timer, until
// ctx ends.
//
// This is what the published root RPO is: without it a machine nobody
// checkpoints has its writes only on one host's NVMe, for as long as it runs,
// which is the "eventual durability with an unpublished window" this repo
// refuses to ship (docs/prior-art/sprites-dev.md REJECT 6). The window is
// Options.RootFlushInterval, PILOT_ROOT_FLUSH_INTERVAL, and a zero interval
// is the operator switching the bound off.
//
// Single-writer (AGENTS.md hard rule 1): a flush writes rootfs_build_id and
// mem_build_id on the machine row, so it may only ever run on the owning
// host, and selectFlushable is where that is enforced.
func (m *Manager) RunRootFlush(ctx context.Context) {
	interval := m.opts.RootFlushInterval
	if interval <= 0 {
		return
	}
	live := metrics.NewLoop("root-flush", 3*interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.flushRoots(ctx)
			live.Tick()
		}
	}
}

// flushRoots starts a flush for every machine due one. Each runs in its own
// goroutine: a flush is an upload, and one slow machine must not hold the
// others' windows open. A machine whose previous flush is still running is
// skipped, not queued.
func (m *Manager) flushRoots(ctx context.Context) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		slog.Error("root flush could not list machines", "err", err)
		return
	}
	for _, id := range m.selectFlushable(rows) {
		if _, busy := m.flushing.LoadOrStore(id, true); busy {
			continue
		}
		go func(id string) {
			defer m.flushing.Delete(id)
			m.flushRoot(ctx, id)
		}(id)
	}
}

// selectFlushable is the choice flushRoots acts on, split out so it can be
// asserted without an engine behind it: this host's own running machines, and
// nobody else's. A builder is skipped too -- it is hostd's machine, its disk
// is a layer cache that is rebuilt rather than restored, and flushing it
// every minute would upload the largest cow on the host for nothing.
func (m *Manager) selectFlushable(rows []state.Machine) []string {
	var due []string
	for _, row := range rows {
		if row.HostID != m.opts.HostID || row.State != StateRunning {
			continue
		}
		if strings.HasPrefix(row.Name, builderNamePrefix) {
			continue
		}
		due = append(due, row.ID)
	}
	return due
}

// flushRoot flushes one machine and moves its row to the flushed disk.
//
// The whole of it runs under the machine's lock, the same lock a suspend or a
// checkpoint takes, so the row it reads is the row it writes. After a flush
// the row names the flushed disk and NO memory image: the image it had was
// taken at the last suspend and describes a disk this one has moved past, and
// pairing it with a newer disk on a rescue is the memory-and-disk-never-met
// corruption the exit path clears for the same reason. A rescue then
// cold-boots the flushed disk, which is the published window as a behaviour:
// what the machine wrote in the last interval is what comes back.
func (m *Manager) flushRoot(ctx context.Context, id string) {
	// TryLock, never Lock. A machine somebody else is holding is a machine
	// whose disk that operation is already making durable -- a suspend, a
	// checkpoint, a resize, a rollout's own capture -- so a flush has nothing
	// to add to it and every reason not to queue behind it. The next tick
	// picks the machine up, which is what a bounded window can afford and a
	// blocked deploy cannot.
	lock := m.lockFor(id)
	if !lock.TryLock() {
		return
	}
	unlock := sync.OnceFunc(lock.Unlock)
	defer unlock()

	fcm, ok := m.get(id)
	if !ok {
		return
	}
	row, err := m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		slog.Warn("root flush could not read the machine's row", "machine", id, "err", err)
		return
	}
	// Re-checked under the lock: the listing that chose this machine is stale
	// by the time the lock is held, and a machine suspended meanwhile has no
	// disk to flush and a row this host must not rewrite.
	if row.State != StateRunning || row.HostID != m.opts.HostID {
		return
	}
	t, err := m.templateFor(ctx, row)
	if err != nil {
		slog.Warn("root flush could not resolve the machine's template", "machine", id, "err", err)
		return
	}
	// The build is diffed against the template's local directory, which may
	// still be hydrating on a host that adopted the template a moment ago.
	// Left for the next tick rather than waited on under the lock.
	if !block.BuildComplete(m.rootfsTemplateDir(t)) {
		return
	}

	started := time.Now()
	opts := m.snapshotOpts(t)
	finish, err := fcm.StageRootFlush(ctx)
	if err != nil {
		slog.Warn("root flush failed; the machine's latest writes are not yet durable",
			"machine", id, "err", err)
		return
	}
	if finish == nil {
		m.dropStaleMemory(ctx, id)
		return
	}

	// Unlocked for the chunkify and the upload. They run with the guest
	// serving, and a suspend or a checkpoint arriving now takes this machine
	// at once and waits only at the capture gate -- which is the difference
	// between a background loop nobody notices and one that holds a deploy
	// for the length of an upload.
	unlock()

	build, err := finish(ctx, m.opts.Chunks, opts)
	if err != nil {
		slog.Warn("root flush failed; the machine's latest writes are not yet durable",
			"machine", id, "err", err)
		return
	}

	// Re-taken, and the row re-read: everything above ran unlocked, so the
	// copy read before the upload would hand last_activity and the state
	// columns back their old values.
	lock.Lock()
	defer lock.Unlock()
	row, err = m.opts.Store.GetMachine(ctx, id)
	if err != nil {
		slog.Warn("root flush could not re-read the machine's row", "machine", id, "err", err)
		m.discardBuilds(ctx, build.String())
		return
	}
	if row.State != StateRunning || row.HostID != m.opts.HostID {
		// Suspended, destroyed or handed over while this flushed. Whatever
		// did that captured the disk itself, so this build is superseded
		// before it was ever named.
		m.discardBuilds(ctx, build.String())
		return
	}

	superseded := []string{row.RootfsBuildID}
	row.RootfsBuildID = build.String()
	stale := row.MemBuildID
	row.MemBuildID = ""
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		slog.Warn("root flush could not record the flushed disk on the row",
			"machine", id, "err", err)
		m.discardBuilds(ctx, build.String())
		return
	}
	slog.Info("root flushed", "machine", id, "build", build,
		"ms", time.Since(started).Milliseconds())
	// Only AFTER the row names the new build, for the reason suspend gives:
	// deleting first would, on a failed write, leave the row pointing at an
	// object that no longer exists.
	m.discardBuilds(ctx, superseded...)
	if stale != "" {
		m.staleMem.Store(id, stale)
	}
	m.dropStaleMemory(ctx, id)
}

// dropStaleMemory deletes a memory image a flush unpinned from the row, once
// nothing on this host can still need it.
//
// The machine is RUNNING, and it may have been woken from that very image:
// its fault handler pages the image in lazily and pulls the rest in the
// background, so the objects can go only once the local copy is complete.
// Until then the id is remembered and retried on the next flush; a suspend
// meanwhile supersedes it with a fresh image and discards it on the way. The
// one leak is a hostd restart in that window, which forgets the id -- one
// image per machine, and bounded, against a fault handler paging from an
// object that is gone.
func (m *Manager) dropStaleMemory(ctx context.Context, id string) {
	v, ok := m.staleMem.Load(id)
	if !ok {
		return
	}
	mem := v.(string)
	if !block.BuildComplete(filepath.Join(m.buildDir(), mem)) {
		return
	}
	m.staleMem.Delete(id)
	// The same deletion a cold boot performs, over a row that names only
	// what is being dropped.
	m.discardMemoryImage(ctx, &state.Machine{ID: id, MemBuildID: mem})()
}
