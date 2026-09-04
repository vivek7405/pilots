package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Manager is the lifecycle surface the handlers drive. An interface rather
// than the concrete type so the API can be tested without booting VMs.
type Manager interface {
	// CollectMetrics folds the per-machine engine counters into the metrics
	// registry. Called on each scrape of GET /metrics, because the memory
	// handlers are separate processes that have to be asked.
	CollectMetrics()

	Create(ctx context.Context, req CreateMachineRequest) (*state.Machine, error)
	Destroy(ctx context.Context, id string) error
	Suspend(ctx context.Context, id string) error
	Wake(ctx context.Context, id string) error
	Checkpoint(ctx context.Context, machineID, comment string) (*state.Checkpoint, error)
	ListCheckpoints(ctx context.Context, machineID string) ([]state.Checkpoint, error)
	RestoreCheckpoint(ctx context.Context, checkpointID string) (*state.Machine, error)
	GetCheckpoint(ctx context.Context, checkpointID string) (*state.Checkpoint, error)
	Exec(ctx context.Context, machineID string, req ExecRequest) (*ExecResponse, error)
	Logs(ctx context.Context, machineID string) ([]byte, error)
	// ExecStream proxies the agent's websocket exec stream onto w. An error is
	// returned only before anything was written (a wake that failed, a machine
	// that is not running); once the upgrade has been attempted it is nil.
	ExecStream(w http.ResponseWriter, r *http.Request, machineID string) error
	// LogsFrom is Logs from a byte offset, for a follow. ErrNotFound once the
	// machine is destroyed; nil, nil when nothing new has been written.
	LogsFrom(ctx context.Context, machineID string, offset int64) ([]byte, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*state.Volume, error)
	ListVolumes(ctx context.Context) ([]state.Volume, error)
	MachineVolume(ctx context.Context, machineID string) (*MachineVolume, error)
}

// toAPI converts a stored row to the wire shape.
//
// The URL is derived from the machine's domain rather than stored twice, so
// there is exactly one place a machine's address is decided.
//
// orgID is passed in rather than looked up here: a list endpoint already knows
// every row's owner from the pass it made to filter them, and re-asking per
// row would turn one lookup into N.
func toAPI(row state.Machine, orgID string) Machine {
	return Machine{
		ID: row.ID, Name: row.Name, HostID: row.HostID, State: row.State,
		OrgID:        orgID,
		Knobs:        ParseKnobs(row.KindKnobs),
		ImageRef:     row.ImageRef,
		VCPUs:        row.VCPUs,
		MemMiB:       row.MemMiB,
		URL:          "https://" + row.Domain,
		CustomDomain: row.CustomDomain,
		VolumeID:     row.VolumeID,
		ServiceID:    row.ServiceID,
		ReleaseID:    row.ReleaseID,
		App:          row.App,
		CreatedAt:    row.UpdatedAt,
		LastActivity: row.LastActivity,
	}
}

func toAPICheckpoint(c state.Checkpoint) Checkpoint {
	return Checkpoint{
		ID: c.ID, MachineID: c.MachineID, Seq: c.Seq, Comment: c.Comment,
		SourceID: c.SourceID, Durable: c.Durable, CreatedAt: c.CreatedAt,
		ResumeGapMS: c.ResumeGapMS,
	}
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// writeErr maps a lifecycle error to a status. A missing machine is a 404
// rather than a 500 so a client can tell "gone" from "broken".
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, state.ErrNotFound):
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}

func (d Deps) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	var req CreateMachineRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request body"})
		return
	}
	// The org comes from the authenticated key and overwrites whatever the
	// body said. CreateMachineRequest.OrgID is `json:"-"` so a body cannot
	// carry one at all, and this line is the only thing that ever sets it.
	req.OrgID = OrgID(r.Context())

	// A create may name a volume to attach, and a volume is another tenant's
	// data. Without this, naming a foreign id in the body would mount someone
	// else's filesystem into a machine the caller controls -- the one place
	// tenancy could be crossed by a create rather than by a read.
	if req.Volume != "" {
		if _, ok := d.ownedVolume(w, r, req.Volume); !ok {
			return
		}
	}

	// And a create may name a built image, which is the same crossing by a
	// different door: the build id becomes this machine's root filesystem.
	if req.Image != "" {
		if !d.ownedBuild(w, r, req.Image) {
			return
		}
	}

	if !d.checkQuota(w, r, quota.Delta{
		Machines: 1,
		VCPUs:    orDefault(req.VCPUs, 1),
		MemMiB:   orDefault(req.MemMiB, 512),
	}) {
		return
	}

	row, err := d.Machines.Create(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPI(*row, req.OrgID))
}

// orDefault mirrors the machine manager's own defaulting, so the quota check
// counts the size the create will really ask for rather than the zero a client
// left out.
func orDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func (d Deps) handleListMachines(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	org, narrow := listOrg(r)
	out := make([]Machine, 0, len(rows))
	for _, row := range rows {
		owner, ok := d.visible(r, row.ID, org, narrow)
		if !ok {
			continue
		}
		out = append(out, toAPI(row, owner))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), row.ID)
	writeJSON(w, http.StatusOK, toAPI(*row, owner))
}

func (d Deps) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
	// Resolved before the destroy, not after: a foreign id must never reach
	// the manager, or a tenant could delete another tenant's machine and be
	// told 404 about a machine that is already gone.
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Destroy(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request body"})
		return
	}
	if req.Cmd == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "cmd is required"})
		return
	}
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	resp, err := d.Machines.Exec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleExecStream(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	d.execStream(w, r, row.ID)
}

// handleSpriteExec is the sprites-compatible alias.
//
// {name} is a machine NAME, which is what a sprites consumer persists as the
// sprite id; an id-shaped value is tried as an id first. Unknown or foreign is
// a 404 like any other id, never a 403: a 403 would say the name exists.
func (d Deps) handleSpriteExec(w http.ResponseWriter, r *http.Request) {
	id, ok := d.machineIDByName(r.Context(), r.PathValue("name"))
	if !ok {
		notFound(w, "machine")
		return
	}
	row, ok := d.ownedMachine(w, r, id)
	if !ok {
		return
	}
	d.execStream(w, r, row.ID)
}

// execStream is the half both stream routes share. Ownership is settled by the
// caller, so a foreign machine is a 404 whether or not the query is well
// formed.
func (d Deps) execStream(w http.ResponseWriter, r *http.Request, id string) {
	if len(r.URL.Query()["cmd"]) == 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "cmd is required"})
		return
	}
	if err := d.Machines.ExecStream(w, r, id); err != nil {
		writeErr(w, err)
	}
}

// machineIDShape matches an id minted by newID("m").
var machineIDShape = regexp.MustCompile(`^m-[0-9a-f]{24}$`)

// machineIDByName resolves the alias's path segment.
//
// An id-shaped value that names a row wins, because that is one row read
// rather than a list scan; anything else is the live machine of that name with
// the lowest id, the rule the router's cache already applies. Two live rows
// with one name is a bug elsewhere, and the lowest id is the stable answer to
// it rather than whichever the store listed first.
func (d Deps) machineIDByName(ctx context.Context, name string) (string, bool) {
	if machineIDShape.MatchString(name) {
		if _, err := d.Store.GetMachine(ctx, name); err == nil {
			return name, true
		}
	}
	rows, err := d.Store.ListMachines(ctx)
	if err != nil {
		return "", false
	}
	id := ""
	for _, row := range rows {
		if row.Name != name || row.State == state.StateDestroyed {
			continue
		}
		if id == "" || row.ID < id {
			id = row.ID
		}
	}
	return id, id != ""
}

func (d Deps) handleSuspend(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Suspend(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleWake(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Wake(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req CheckpointRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request body"})
		return
	}
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	ckpt, err := d.Machines.Checkpoint(r.Context(), r.PathValue("id"), req.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPICheckpoint(*ckpt))
}

func (d Deps) handleListCheckpoints(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	cks, err := d.Machines.ListCheckpoints(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]Checkpoint, 0, len(cks))
	for _, c := range cks {
		out = append(out, toAPICheckpoint(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRestoreCheckpoint restores in place: same machine, same URL, same
// token.
//
// A checkpoint id is resolved to its machine BEFORE anything acts on it, and
// the machine is what tenancy is checked against: checkpoints carry no org of
// their own, so a foreign checkpoint id is a foreign machine.
func (d Deps) handleRestoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	ck, err := d.Machines.GetCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, ok := d.ownedMachine(w, r, ck.MachineID); !ok {
		return
	}
	row, err := d.Machines.RestoreCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), row.ID)
	writeJSON(w, http.StatusOK, toAPI(*row, owner))
}

// handleCheckpointStatus lets a caller learn when a checkpoint became durable.
//
// Checkpoint returns as soon as the guest is running again, with the upload
// still in flight, so this is the only way to know the data can be restored
// from another host.
func (d Deps) handleCheckpointStatus(w http.ResponseWriter, r *http.Request) {
	ck, err := d.Machines.GetCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, ok := d.ownedMachine(w, r, ck.MachineID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, toAPICheckpoint(*ck))
}

// logFollowInterval is how often a follow looks for new console output.
//
// A poll rather than inotify: it is one bounded read of one file per open
// tail, and the consumer is a human reading a pane or an agent tailing a boot.
// Nothing reads faster than this, and nothing here is a dependency.
const logFollowInterval = 500 * time.Millisecond

func (d Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	id := r.PathValue("id")
	logs, err := d.Machines.Logs(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)

	if !r.URL.Query().Has("follow") { // follow=1 and a bare follow both work
		return
	}
	d.followLogs(w, r, id, int64(len(logs)))
}

// followLogs streams the console log as it grows.
//
// It ends on exactly two things: the client leaving, and the machine being
// destroyed. NOT on suspend -- the idle monitor suspends a quiet sandbox after
// a minute, so a follow that ended there would cut every agent's tail one
// minute into a session. A suspended machine keeps its state dir, so the file
// stays where it is and the follow simply sees no delta until the wake.
func (d Deps) followLogs(w http.ResponseWriter, r *http.Request, id string, offset int64) {
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	ticker := time.NewTicker(logFollowInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}

		delta, err := d.Machines.LogsFrom(r.Context(), id, offset)
		if errors.Is(err, state.ErrNotFound) {
			return // destroyed: the row is gone, and so is the file
		}
		if err != nil {
			// A read that failed once is not a reason to end a tail the caller
			// cannot restart from where it stopped.
			slog.Warn("log follow read failed; will retry", "machine", id, "err", err)
			continue
		}
		if len(delta) == 0 {
			continue
		}
		if _, err := w.Write(delta); err != nil {
			return
		}
		flush()
		offset += int64(len(delta))
	}
}

// toAPIVolume converts a stored volume row to the wire shape.
//
// Sizes are stored in mebibytes and reported in gibibytes, because a volume is
// created in gibibytes and the two must round-trip: a 10 GiB volume that comes
// back as 10240 of something is a client bug waiting to happen.
func toAPIVolume(v state.Volume, orgID string) Volume {
	return Volume{
		ID: v.ID, Name: v.Name, OrgID: orgID, SizeGiB: v.SizeMiB / 1024,
		MachineID: v.MachineID, HostID: v.HostID, MountPath: v.MountPath,
		CreatedAt: v.CreatedAt,
	}
}

func (d Deps) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req CreateVolumeRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request body"})
		return
	}
	if req.SizeGiB <= 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "size_gib is required"})
		return
	}
	req.OrgID = OrgID(r.Context())
	if !d.checkQuota(w, r, quota.Delta{VolumeGiB: req.SizeGiB}) {
		return
	}
	v, err := d.Machines.CreateVolume(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(*v, req.OrgID))
}

func (d Deps) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Machines.ListVolumes(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	org, narrow := listOrg(r)
	out := make([]Volume, 0, len(rows))
	for _, v := range rows {
		owner, ok := d.visible(r, v.ID, org, narrow)
		if !ok {
			continue
		}
		out = append(out, toAPIVolume(v, owner))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMachineVolume reports the volume drive Firecracker is really running.
func (d Deps) handleMachineVolume(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	v, err := d.Machines.MachineVolume(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (d Deps) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := d.Store.ListHosts(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	const aliveWindow = 30 * time.Second
	out := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, Host{
			ID: h.ID, PublicIP: h.PublicIP, WGAddr: h.WGAddr,
			CPUFree: h.CPUFree, MemFreeMiB: h.MemFreeMiB, LastSeen: h.LastSeen,
			Alive: time.Since(time.Unix(h.LastSeen, 0)) < aliveWindow,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
