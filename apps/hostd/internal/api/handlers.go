package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Manager is the lifecycle surface the handlers drive. An interface rather
// than the concrete type so the API can be tested without booting VMs.
type Manager interface {
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

func (d Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	logs, err := d.Machines.Logs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)
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
