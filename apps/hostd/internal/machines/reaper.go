package machines

import (
	"context"
	"github.com/google/uuid"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/pilotsrun/pilots/hostd/internal/metrics"
	"github.com/pilotsrun/pilots/hostd/internal/netns"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Reaper timings.
//
// The age guard is the important one: a machine that is mid-create has a live
// Firecracker but has not yet written its row, and reaping it would destroy a
// machine the caller is still waiting on. A minute is far longer than a create
// takes and far shorter than anyone would tolerate an orphan lingering.
//
// The loop is slow because killing a Firecracker is destructive and a false
// positive is visible to a user; there is no hurry.
const (
	reaperMinAge   = 60 * time.Second
	reaperInterval = 5 * time.Minute
)

// RunReaper kills Firecracker processes this host has no record of, until ctx
// ends.
//
// Orphans happen: hostd can be SIGKILLed between spawning a machine and
// recording it, or a destroy can fail partway. Without a sweep they hold their
// memory, their cgroup and their network slot indefinitely, and the only
// remedy is a human with a terminal.
func (m *Manager) RunReaper(ctx context.Context) {
	live := metrics.NewLoop("reaper", 3*reaperInterval)
	ticker := time.NewTicker(reaperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapOrphans(ctx)
			// The same loop, for the same reason: this is where "clean up what
			// nothing is using" already lives, and a second timer for
			// checkpoints would be a second thing to reason about.
			m.ExpireCheckpoints(ctx)
			m.sweepOrphanBuilds(ctx)
			live.Tick()
		}
	}
}

func (m *Manager) reapOrphans(ctx context.Context) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		slog.Error("reaper could not list machines", "err", err)
		return
	}
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		known[r.ID] = true
	}

	for _, p := range firecrackerProcesses() {
		if known[p.machineID] {
			continue
		}
		if p.age < reaperMinAge {
			// Probably a create still in flight.
			continue
		}
		slog.Warn("reaping an orphaned firecracker with no machine record",
			"pid", p.pid, "machine", p.machineID, "age", p.age)
		if err := unix.Kill(p.pid, unix.SIGKILL); err != nil {
			slog.Error("could not reap orphan", "pid", p.pid, "err", err)
			continue
		}
		// Killing the process is not the whole job: its namespace, veth,
		// chroot and cgroup outlive it, and a namespace left behind blocks the
		// next machine that lands on the same slot.
		m.reapOrphanResources(p.machineID)
	}
}

// reapOrphanResources removes what an orphaned machine left on the host.
func (m *Manager) reapOrphanResources(machineID string) {
	// The namespace is named after the machine, so it can be torn down without
	// knowing which slot it held.
	if err := netns.TeardownByName(machineID); err != nil {
		slog.Warn("could not tear down an orphan's namespace",
			"machine", machineID, "err", err)
	}
	if err := os.RemoveAll(filepath.Join(m.opts.FCConfig.ChrootBase, "firecracker", machineID)); err != nil {
		slog.Warn("could not remove an orphan's chroot", "machine", machineID, "err", err)
	}
	if err := os.RemoveAll(m.stateDir(machineID)); err != nil {
		slog.Warn("could not remove an orphan's state dir", "machine", machineID, "err", err)
	}
	// An empty cgroup still costs a kernel structure, so a host that made one
	// per machine and removed none would accumulate them for as long as it
	// stays up.
	m.killCgroup(machineID)
	m.removeCgroup(machineID)
}

type fcProcess struct {
	pid       int
	machineID string
	age       time.Duration
}

// firecrackerProcesses finds running Firecrackers and the machine each claims
// to be, read from the --id it was started with.
func firecrackerProcesses() []fcProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var out []fcProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "firecracker" {
			continue
		}

		id := machineIDFromCmdline(filepath.Join("/proc", e.Name(), "cmdline"))
		if id == "" {
			continue
		}

		age := time.Duration(0)
		if st, err := os.Stat(filepath.Join("/proc", e.Name())); err == nil {
			age = time.Since(st.ModTime())
		}
		out = append(out, fcProcess{pid: pid, machineID: id, age: age})
	}
	return out
}

// machineIDFromCmdline reads the --id Firecracker was started with. The
// jailer passes it through, so it is the machine's own identifier.
func machineIDFromCmdline(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	args := strings.Split(string(raw), "\x00")
	for i, a := range args {
		if a == "--id" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// orphanBuildAge is how old a build directory nothing references must be
// before the sweep removes it. Generous, because a build is on disk before
// the row that will name it exists -- a create pulling its template, a
// checkpoint chunkifying, a flush uploading -- and none of those take hours.
const orphanBuildAge = 2 * time.Hour

// sweepOrphanBuilds removes this host's build directories that no row names.
//
// Every path that supersedes a build removes its directory, but nothing ever
// walked the directory for what those paths missed: a build whose machine
// was destroyed on another host's say-so, a checkpoint expired before its
// row was read here, a flush whose row write failed after the upload. On a
// laptop that ran four batteries in a day that was 1,700 directories and
// 134 GB, on a filesystem then at 98%, and every snapshot write paid for it.
// Local only: the bucket is shared and another host may still name what
// this one does not.
func (m *Manager) sweepOrphanBuilds(ctx context.Context) {
	referenced, ok := m.referencedBuilds(ctx)
	if !ok {
		return
	}
	entries, err := os.ReadDir(m.buildDir())
	if err != nil {
		return
	}
	var dirs []buildDirInfo
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, buildDirInfo{id: e.Name(), age: now.Sub(info.ModTime())})
	}
	removed := 0
	for _, id := range selectOrphanBuilds(dirs, referenced) {
		if err := os.RemoveAll(filepath.Join(m.buildDir(), id)); err != nil {
			slog.Warn("could not remove an orphaned build directory", "build", id, "err", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		slog.Info("swept build directories nothing references", "count", removed)
	}
}

type buildDirInfo struct {
	id  string
	age time.Duration
}

// selectOrphanBuilds is the choice sweepOrphanBuilds acts on: directories
// no row names, old enough that no operation can still be about to name
// them.
func selectOrphanBuilds(dirs []buildDirInfo, referenced map[string]bool) []string {
	var out []string
	for _, d := range dirs {
		if referenced[d.id] || d.age < orphanBuildAge {
			continue
		}
		out = append(out, d.id)
	}
	return out
}

// referencedBuilds is every build id a row on this host can name: a
// machine's own disk and memory, the template it was created from, the image
// it booted, its checkpoints, every release of every service, and the
// templates this host adopted. False when any of those could not be read, in
// which case nothing is swept: a listing that failed is not a listing that
// was empty.
func (m *Manager) referencedBuilds(ctx context.Context) (map[string]bool, bool) {
	ref := map[string]bool{}
	add := func(ids ...string) {
		for _, id := range ids {
			if id != "" && id != uuid.Nil.String() {
				ref[id] = true
			}
		}
	}
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, false
	}
	for _, r := range rows {
		add(r.RootfsBuildID, r.MemBuildID, r.TemplateMemBuildID, r.TemplateRootfsBuildID, r.ImageRef)
		cks, err := m.opts.Store.ListCheckpoints(ctx, r.ID)
		if err != nil {
			return nil, false
		}
		for _, c := range cks {
			add(c.RootfsBuildID, c.MemBuildID)
		}
	}
	svcs, err := m.opts.Store.ListServiceNames(ctx)
	if err != nil {
		return nil, false
	}
	for _, s := range svcs {
		rels, err := m.opts.Store.ReleasesFor(ctx, s.ID)
		if err != nil {
			return nil, false
		}
		for _, rel := range rels {
			add(rel.RootfsBuildID, rel.MemBuildID)
		}
	}
	for _, v := range []variant{variantGolden, variantBuilder} {
		if t, err := m.loadTemplateManifest(v); err == nil && t != nil {
			add(t.MemBuildID.String(), t.RootfsBuildID.String())
		}
	}
	return ref, true
}
