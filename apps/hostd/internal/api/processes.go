package api

import (
	"net/http"
	"strconv"
)

// A machine's processes, over the public API.
//
// Every handler here is owner-routed the way the rest of the machine surface
// is: ownedMachine resolves the row and refuses another org's, and the manager
// forwards to the host that holds the machine. The body the guest produced is
// passed through rather than re-encoded, because the guest is the only thing
// that knows whether a pid is alive.

func (d Deps) handleProcesses(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
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
