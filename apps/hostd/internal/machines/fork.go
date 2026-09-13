package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Making a new machine out of an existing one's exact state.
//
// # What a fork is
//
// A NEW machine -- new id, new name, new URL, new agent token -- restored from
// another machine's memory and disk. Not a copy of a disk image: the fork comes
// up with the parent's processes already running, its memory already warm, its
// open files where they were. An agent that has spent two minutes installing
// dependencies and loading a model can be forked into ten machines that all
// begin from that moment.
//
// # Why it costs almost nothing
//
// The mechanism already existed and was doing this daily. A release restores N
// replicas from one snapshot; a checkpoint and a release are the same artifact
// pair by design. So a fork is that same restore, addressed by a machine rather
// than by a release, with a row sequence of its own.
//
// `CreateMachineRequest.Checkpoint` has been on the wire since the beginning
// and read by nothing -- the same dead field `Template` was. This is what it
// was for.
//
// # The two sources, and why suspended is the interesting one
//
// A RUNNING machine is checkpointed first, in place, so the source keeps its
// id, URL and slot. A SUSPENDED machine needs NO wake at all: its suspend image
// is already a restorable point, so forking one costs the source nothing and
// wakes nobody. e2b refuses to fork a paused sandbox; this is the case where
// that difference shows.
//
// # What pins the parent's builds
//
// A fork faults pages out of the artifacts it was restored from until its own
// first suspend writes its own. The lineage row records which, and
// buildReferenced reads it before anything discards a build -- otherwise the
// parent's next suspend would delete an object a live machine is reading, and
// the failure would surface as an unrelated guest hanging on a page fault.

// The request and result shapes live in the API package, alongside every other
// wire type this package takes. ForkOptions carries no source the caller could
// name themselves: the API resolves the machine or checkpoint and checks who
// owns it first, so a body cannot point at somebody else's.

// Fork makes count new machines from one machine or checkpoint.
func (m *Manager) Fork(ctx context.Context, req api.ForkOptions) ([]api.ForkOutcome, error) {
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > api.MaxForkCount {
		return nil, fmt.Errorf("%w: %d forks at once is more than the %d a request may ask for",
			ErrInvalid, req.Count, api.MaxForkCount)
	}
	if (req.Machine == "") == (req.Checkpoint == "") {
		return nil, fmt.Errorf("%w: name exactly one of a machine or a checkpoint to fork",
			ErrInvalid)
	}

	source, err := m.resolveForkSource(ctx, req)
	if err != nil {
		return nil, err
	}

	// In parallel, because the expensive part of each is a restore and they do
	// not contend: every fork has its own slot, its own jailer root and its own
	// handlers. Bounded by the count, which is bounded above.
	results := make([]api.ForkOutcome, req.Count)
	var wg sync.WaitGroup
	for i := range req.Count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row, err := m.forkOnce(ctx, req, source, i)
			results[i] = api.ForkOutcome{Machine: row, Err: err}
		}(i)
	}
	wg.Wait()
	return results, nil
}

// forkSource is what every fork of one request restores from.
type forkSource struct {
	ParentID      string
	CheckpointID  string
	MemBuildID    string
	RootfsBuildID string
	// TemplateMem and TemplateRootfs are the template the artifacts were
	// diffed against. A restore resolves unchanged ranges against them, so a
	// fork carrying the wrong pair reads another template's pages: silent
	// guest-memory corruption rather than an error.
	TemplateMem    string
	TemplateRootfs string
	VCPUs          int
	MemMiB         int
	App            string
	VolumeID       string
	// VolumeSnapshot is the point the forked volume is filled from, taken at
	// the same moment as the memory image so the two agree.
	VolumeSnapshot string
	// SnapKey is the vmstate the forks restore: device state and vcpu
	// registers, captured in the same instant as MemBuildID.
	//
	// The build ids cannot name it. It is keyed by the machine and checkpoint
	// it was captured from, so it has to be carried from the moment the source
	// is resolved, when both are still in hand. Every fork of one source
	// restores the same one, which is what makes them the same machine.
	SnapKey string
}

// resolveForkSource turns a request into the artifacts every fork restores.
func (m *Manager) resolveForkSource(ctx context.Context, req api.ForkOptions) (*forkSource, error) {
	if req.Checkpoint != "" {
		ck, err := m.findCheckpoint(ctx, req.Checkpoint)
		if err != nil {
			return nil, err
		}
		row, err := m.opts.Store.GetMachine(ctx, ck.MachineID)
		if err != nil {
			return nil, fmt.Errorf("machines: the machine checkpoint %s came from: %w",
				req.Checkpoint, err)
		}
		return &forkSource{
			ParentID: ck.MachineID, CheckpointID: ck.ID,
			MemBuildID: ck.MemBuildID, RootfsBuildID: ck.RootfsBuildID,
			SnapKey:     checkpointSnapKey(ck.MachineID, ck.ID),
			TemplateMem: row.TemplateMemBuildID, TemplateRootfs: row.TemplateRootfsBuildID,
			VCPUs: row.VCPUs, MemMiB: row.MemMiB, App: row.App, VolumeID: row.VolumeID,
		}, nil
	}

	row, err := m.opts.Store.GetMachine(ctx, req.Machine)
	if err != nil {
		return nil, err
	}
	if row.VolumeID != "" && !req.Volume {
		return nil, fmt.Errorf("%w: %s has a volume; fork it with volume: true, or "+
			"the fork comes up with memory expecting a disk it does not have",
			api.ErrConflict, req.Machine)
	}

	switch row.State {
	case StateRunning:
		// Checkpointed in place: the source keeps its id, its URL and its slot,
		// and the checkpoint is the exact moment every fork begins from.
		//
		// NOT under this function's own lock. Checkpoint takes the machine's
		// lock itself, and a sync.Mutex is not reentrant, so holding it here
		// would deadlock on the very path this feature exists for. The
		// checkpoint IS the consistency guarantee: it names a fixed pair of
		// artifacts that a later suspend cannot move.
		ck, err := m.Checkpoint(ctx, req.Machine, "fork")
		if err != nil {
			return nil, fmt.Errorf("machines: checkpoint %s to fork it: %w", req.Machine, err)
		}
		// Checkpoint returns as soon as the artifacts are STAGED; the upload
		// runs behind it. A fork restores from object storage, so starting one
		// now races that upload and loses:
		//
		//   s3: not found: fc: artifact missing: .../checkpoints/<id>/snap.bin
		//
		// The wait belongs here rather than inside Checkpoint, which is right
		// to return early for every caller that only wants a rollback point on
		// this host.
		if fcm, ok := m.get(req.Machine); ok {
			fcm.AwaitCapture(captureDrain)
		}
		fresh, err := m.opts.Store.GetMachine(ctx, req.Machine)
		if err != nil {
			return nil, err
		}
		return &forkSource{
			ParentID: req.Machine, CheckpointID: ck.ID,
			MemBuildID: ck.MemBuildID, RootfsBuildID: ck.RootfsBuildID,
			SnapKey:     checkpointSnapKey(req.Machine, ck.ID),
			TemplateMem: fresh.TemplateMemBuildID, TemplateRootfs: fresh.TemplateRootfsBuildID,
			VCPUs: fresh.VCPUs, MemMiB: fresh.MemMiB, App: fresh.App, VolumeID: fresh.VolumeID,
		}, nil

	case StateSuspended:
		// No wake. A suspend image IS a restorable point, so forking a sleeping
		// machine costs the source nothing and wakes nobody -- the case e2b
		// refuses outright.
		//
		// The source's lock IS held here, for the read: a suspend racing this
		// rewrites the machine's snapshot key, and a fork resolved across that
		// would restore from a memory image and a disk that no longer describe
		// each other. Nothing under the lock calls back into a path that takes
		// it again.
		lock := m.lockFor(req.Machine)
		lock.Lock()
		defer lock.Unlock()

		fresh, err := m.opts.Store.GetMachine(ctx, req.Machine)
		if err != nil {
			return nil, err
		}
		if fresh.MemBuildID == "" {
			return nil, fmt.Errorf("%w: %s is suspended with no memory image, so there "+
				"is nothing to fork from", api.ErrConflict, req.Machine)
		}
		// The SUSPEND image, not a checkpoint: a suspended machine's vmstate
		// lives under its own id and is rewritten by each suspend. Read under
		// the lock held above, with the build ids it belongs to, so the three
		// artifacts a fork restores describe one instant. A suspend racing
		// this would otherwise pair new registers with an old memory image.
		return &forkSource{
			ParentID:   req.Machine,
			MemBuildID: fresh.MemBuildID, RootfsBuildID: fresh.RootfsBuildID,
			SnapKey:     suspendSnapKey(req.Machine),
			TemplateMem: fresh.TemplateMemBuildID, TemplateRootfs: fresh.TemplateRootfsBuildID,
			VCPUs: fresh.VCPUs, MemMiB: fresh.MemMiB, App: fresh.App, VolumeID: fresh.VolumeID,
		}, nil

	default:
		return nil, fmt.Errorf("%w: %s is %s; fork a running or suspended machine",
			api.ErrConflict, req.Machine, row.State)
	}
}

// parentGate is the URL mode the parent was set to, or "" when it has none.
//
// The distinction that matters is between "no row" and "could not ask". No row
// is a real answer and means public; a store that failed is not an answer at
// all, and reading it as public is how a gated machine gets published by a
// transient fault.
func (m *Manager) parentGate(ctx context.Context, parentID string) (string, error) {
	u, err := m.opts.Store.GetURLAuth(ctx, parentID)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if u == nil {
		return "", nil
	}
	return u.Mode, nil
}

// destroyUngated removes a fork that could not be given its parent's gate.
//
// Best effort, and loud either way: if the destroy also fails there is an open
// machine holding somebody's data and the only thing left to do is say so by
// name, so an operator can remove it.
func (m *Manager) destroyUngated(ctx context.Context, forkID, parentID string, cause error) {
	if err := m.Destroy(context.WithoutCancel(ctx), forkID); err != nil {
		slog.Error("a fork could not inherit its parent's URL gate AND could not be "+
			"destroyed; it is PUBLIC and holds its parent's data -- destroy it",
			"fork", forkID, "parent", parentID, "cause", cause, "err", err)
		return
	}
	slog.Warn("destroyed a fork that could not be given its parent's URL gate",
		"fork", forkID, "parent", parentID, "err", cause)
}

// forkOnce makes one machine from the resolved source.
//
// Deliberately Create's own path: the fork is a create whose image happens to
// be a machine's memory rather than a build. Everything a create does for a
// machine -- the name, the token, the tenancy row, the slot, the quota already
// checked above it -- a fork needs identically.
func (m *Manager) forkOnce(ctx context.Context, req api.ForkOptions, src *forkSource, i int) (*state.Machine, error) {
	name := req.Name
	if name != "" && i > 0 {
		name = fmt.Sprintf("%s-%d", name, i+1)
	}

	create := api.CreateMachineRequest{
		Name:          name,
		OrgID:         req.OrgID,
		App:           src.App,
		VCPUs:         src.VCPUs,
		MemMiB:        src.MemMiB,
		MemBuildID:    src.MemBuildID,
		RootfsBuildID: src.RootfsBuildID,
		// Without this the restore fetches an empty object key and dies inside
		// the AWS SDK, having named neither the fork nor its source.
		MemSnapKey: src.SnapKey,
		// A fork's image is a picture of a LIVE machine, so it carries that
		// machine's credential rather than the placeholder a release image is
		// reset to. Authenticating as the placeholder is a 401.
		ImageToken: m.token(src.ParentID),
	}
	row, err := m.Create(ctx, create)
	if err != nil {
		return nil, err
	}

	// The parent's URL GATE comes with it.
	//
	// A fork is a new machine with a new URL, but it is the parent's memory and
	// the parent's disk: the same data, reachable at a new address. A parent
	// somebody deliberately set to `org` forked into a machine with no url_auth
	// row, and no row reads as public -- so forking a gated machine published
	// its contents to anyone who had the fork's URL, silently, and the fork
	// looked exactly like a machine whose owner had chosen public.
	//
	// Copied rather than inherited by lookup, because the router reads one row
	// per object on the hot path and a chain to walk there is a chain to walk
	// on every request.
	//
	// Not best effort, and this is where it used to be. The read was
	// `err == nil && ...`, so a store that could not answer produced a fork
	// with no url_auth row -- and no row reads as PUBLIC. A read failure, the
	// one moment nothing knows whether the parent was gated, was the moment
	// the parent's data was published. The write failure was logged and
	// carried on, under a comment claiming it was "refused loudly".
	//
	// Both now destroy the fork and fail the request. A fork that cannot be
	// proved to carry its parent's gate must not exist: the parent's memory
	// and the parent's disk are already inside it, and an open machine is not
	// something to leave running while somebody reads a log.
	mode, err := m.parentGate(ctx, src.ParentID)
	if err != nil {
		m.destroyUngated(ctx, row.ID, src.ParentID, err)
		return nil, fmt.Errorf("machines: could not read the URL gate of %s, so this "+
			"fork would be public without anything knowing whether its parent is: %w",
			src.ParentID, err)
	}
	if mode != "" && mode != api.URLAuthPublic {
		if err := m.opts.Store.PutURLAuth(ctx, &state.URLAuth{
			ID: row.ID, Kind: "machine", Mode: mode, UpdatedAt: time.Now().Unix(),
		}); err != nil {
			m.destroyUngated(ctx, row.ID, src.ParentID, err)
			return nil, fmt.Errorf("machines: could not give this fork the %s gate its "+
				"parent %s has: %w", mode, src.ParentID, err)
		}
	}

	// The lineage row LAST, after the machine exists, and best effort on the
	// error. A fork with no lineage row still runs; what it loses is the
	// pinning that keeps its parent's builds alive, and that is worth shouting
	// about rather than failing a machine that is already serving.
	lineage := &state.Lineage{
		ID: row.ID, ParentID: src.ParentID, CheckpointID: src.CheckpointID,
		MemBuildID: src.MemBuildID, RootfsBuildID: src.RootfsBuildID,
		VolumeSnapshot: src.VolumeSnapshot, CreatedAt: time.Now().Unix(),
	}
	if err := m.opts.Store.PutLineage(ctx, lineage); err != nil {
		slog.Error("a fork has no lineage row, so its parent's builds are not pinned; "+
			"suspending or destroying the parent may break it",
			"fork", row.ID, "parent", src.ParentID, "err", err)
	}
	slog.Info("forked a machine",
		"fork", row.ID, "parent", src.ParentID, "checkpoint", src.CheckpointID)
	return row, nil
}

// Lineage is where a machine came from, or nil when it was not forked.
func (m *Manager) Lineage(ctx context.Context, machineID string) *state.Lineage {
	l, err := m.opts.Store.GetLineage(ctx, machineID)
	if err != nil {
		return nil
	}
	return l
}
