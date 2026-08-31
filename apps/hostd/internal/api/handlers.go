package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

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
	Exec(ctx context.Context, machineID string, req ExecRequest) (*ExecResponse, error)
	Logs(ctx context.Context, machineID string) ([]byte, error)
}

// toAPI converts a stored row to the wire shape.
//
// The URL is derived from the machine's domain rather than stored twice, so
// there is exactly one place a machine's address is decided.
func toAPI(row state.Machine) Machine {
	return Machine{
		ID: row.ID, Name: row.Name, HostID: row.HostID, State: row.State,
		Knobs:        ParseKnobs(row.KindKnobs),
		ImageRef:     row.ImageRef,
		VCPUs:        row.VCPUs,
		MemMiB:       row.MemMiB,
		URL:          "https://" + row.Domain,
		CustomDomain: row.CustomDomain,
		VolumeID:     row.VolumeID,
		ServiceID:    row.ServiceID,
		ReleaseID:    row.ReleaseID,
		CreatedAt:    row.UpdatedAt,
		LastActivity: row.LastActivity,
	}
}

func toAPICheckpoint(c state.Checkpoint) Checkpoint {
	return Checkpoint{
		ID: c.ID, MachineID: c.MachineID, Seq: c.Seq, Comment: c.Comment,
		SourceID: c.SourceID, Durable: c.Durable, CreatedAt: c.CreatedAt,
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
	row, err := d.Machines.Create(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPI(*row))
}

func (d Deps) handleListMachines(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]Machine, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAPI(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	row, err := d.Store.GetMachine(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPI(*row))
}

func (d Deps) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
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
	resp, err := d.Machines.Exec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleSuspend(w http.ResponseWriter, r *http.Request) {
	if err := d.Machines.Suspend(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleWake(w http.ResponseWriter, r *http.Request) {
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
	ckpt, err := d.Machines.Checkpoint(r.Context(), r.PathValue("id"), req.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPICheckpoint(*ckpt))
}

func (d Deps) handleListCheckpoints(w http.ResponseWriter, r *http.Request) {
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

func (d Deps) handleRestoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	row, err := d.Machines.RestoreCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPI(*row))
}

func (d Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
	logs, err := d.Machines.Logs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)
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
