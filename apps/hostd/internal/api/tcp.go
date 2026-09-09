package api

import (
	"net/http"
	"strconv"
)

// handleTCPStream is GET /v1/machines/{id}/tcp/{port}: one TCP connection
// to a port inside the machine, as binary websocket frames. `pilot proxy`
// opens one per accepted local connection. The machine is woken if it is
// suspended, the way an exec stream wakes it.
func (d Deps) handleTCPStream(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "port must be 1-65535", "pass the port the process inside listens on", nil)
		return
	}
	if err := d.Machines.TCPStream(w, r, row.ID, port); err != nil {
		writeMapped(w, err)
	}
}
