package api

import (
	"net/http"
)

// Terminal sessions: what a machine has open, attach to one, end one. The
// list and the kill are answered by the guest agent verbatim; attach is a
// websocket carrying the exec stream's frames.

func (d Deps) handleListSessions(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	body, err := d.Machines.SessionsJSON(r.Context(), row.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (d Deps) handleAttachSession(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := d.Machines.AttachStream(w, r, row.ID, r.PathValue("session")); err != nil {
		writeMapped(w, err)
	}
}

func (d Deps) handleKillSession(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := d.Machines.KillSession(r.Context(), row.ID, r.PathValue("session")); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"killed": r.PathValue("session")})
}
