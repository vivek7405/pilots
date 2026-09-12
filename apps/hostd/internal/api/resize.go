package api

import (
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/quota"
)

// Resizing a machine, over the public API.
//
// A machine's size was decided once, at create, and could never change. The
// only way to give an application more memory was to destroy it and make
// another, which means a new id, a new URL and whatever was on its disk gone.
// Every platform this one is measured against has had vertical scaling for
// years, and the absence was not a design decision.

func (d Deps) handleResizeMachine(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req ResizeMachineRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body",
			"send {\"vcpus\": n} or {\"mem_mib\": n}, or both", nil)
		return
	}
	if req.VCPUs == 0 && req.MemMiB == 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "nothing to change",
			"pass vcpus, mem_mib, or both", nil)
		return
	}

	// Only the INCREASE counts against the quota. A caller shrinking a machine
	// must not be refused for being over a limit it is in the middle of
	// getting under, which is the one moment the limit is most in the way.
	delta := quota.Delta{}
	if req.VCPUs > row.VCPUs {
		delta.VCPUs = req.VCPUs - row.VCPUs
	}
	if req.MemMiB > row.MemMiB {
		delta.MemMiB = req.MemMiB - row.MemMiB
	}
	if delta.VCPUs > 0 || delta.MemMiB > 0 {
		if err := quota.Check(r.Context(), d.Store, actingOrg(r), delta); err != nil {
			if writeQuotaError(w, err) {
				return
			}
			writeMapped(w, err)
			return
		}
	}

	out, err := d.Machines.Resize(r.Context(), row.ID, req.VCPUs, req.MemMiB)
	if err != nil {
		writeMapped(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), out.ID)
	writeJSON(w, http.StatusOK, d.toAPI(*out, owner, d.startOf(r.Context(), out.ID),
		d.labelsOf(r.Context(), out.ID), d.urlAuthOf(r.Context(), out.ID)))
}
