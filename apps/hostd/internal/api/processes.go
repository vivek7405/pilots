package api

import (
	"net/http"
	"strconv"
)

// A machine's processes, over the public API.
//
// Every handler here is owner-routed: ownedMachine resolves the row and refuses
// another org's, and forwardToHost sends the call to the host that actually
// holds the machine. The forward is not a convenience -- the manager talks to
// the agent on a LOCAL netns slot, so on any other host the dial has nothing to
// reach and the wake-if-absent fallback would try to bring the machine up here,
// beside the copy its owner is already running. The body the guest produced is
// passed through rather than re-encoded, because the guest is the only thing
// that knows whether a pid is alive.

func (d Deps) handleProcesses(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToHost(w, r, row.HostID) {
		return
	}
	body, err := d.Machines.Processes(r.Context(), row.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (d Deps) handleProcessAction(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToHost(w, r, row.HostID) {
		return
	}
	if err := d.Machines.ProcessAction(r.Context(), row.ID,
		r.PathValue("name"), r.PathValue("action")); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d Deps) handleProcessLogs(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToHost(w, r, row.HostID) {
		return
	}
	// A bad tail is not worth a 400: it means "give me everything", which is
	// what an absent one means too.
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	body, err := d.Machines.ProcessLogs(r.Context(), row.ID, r.PathValue("name"), tail)
	if err != nil {
		writeMapped(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
