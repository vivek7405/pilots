package api

import (
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Making new machines out of an existing one's exact state.
//
// A fork comes up with the source's processes already running and its memory
// already warm. An agent that spent two minutes installing dependencies and
// loading a model forks into ten machines that all begin from that moment,
// which is the shape this platform is for and the one thing a plain create
// cannot express.

// ForkRequest asks for N copies of a machine or checkpoint.
type ForkRequest struct {
	// Name is the first fork's name; the rest take a suffix. Empty mints one.
	Name string `json:"name,omitempty"`
	// Count is how many, default 1, capped at 100.
	Count int `json:"count,omitempty"`
	// Volume forks the source's volume too. A source WITH a volume and this
	// unset is refused rather than forked without it: a machine restored
	// without the disk its memory expects fails in a way that looks like the
	// application rather than like the fork.
	Volume bool `json:"volume,omitempty"`
}

// ForkResponse is one fork per entry, in the order they were asked for.
//
// A per-fork result rather than one status for the request, because forks are
// independent: nine that came up are worth having when the tenth did not.
type ForkResponse struct {
	Forks []ForkEntry `json:"forks"`
}

// ForkEntry is one fork: the machine, or why it did not happen.
type ForkEntry struct {
	Machine *Machine `json:"machine,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func (d Deps) handleForkMachine(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	d.fork(w, r, state.Lineage{ParentID: row.ID})
}

// handleForkCheckpoint forks from a checkpoint rather than from the machine's
// current state.
//
// The tenancy check is on the checkpoint's MACHINE, which is the door the dead
// `checkpoint` field on CreateMachineRequest never had: a checkpoint id is not
// itself an owned object, so without this a caller could name somebody else's
// checkpoint and get a machine with their data in it.
func (d Deps) handleForkCheckpoint(w http.ResponseWriter, r *http.Request) {
	ck, err := d.Machines.GetCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	if _, ok := d.ownedMachine(w, r, ck.MachineID); !ok {
		return
	}
	d.fork(w, r, state.Lineage{CheckpointID: ck.ID})
}

// fork is the shared half: decode, admit, run, render.
func (d Deps) fork(w http.ResponseWriter, r *http.Request, from state.Lineage) {
	var req ForkRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > MaxForkCount {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"count is more than a request may ask for",
			"ask for at most 100 forks per request", nil)
		return
	}
	// Charged once, for all of them, BEFORE any is made. Charging per fork
	// would let a request that the org cannot afford still create most of the
	// machines before being refused.
	if !d.checkQuota(w, r, quota.Delta{Machines: req.Count}) {
		return
	}

	results, err := d.Machines.Fork(r.Context(), ForkOptions{
		Machine: from.ParentID, Checkpoint: from.CheckpointID,
		Name: req.Name, Count: req.Count, Volume: req.Volume,
		OrgID: actingOrg(r),
	})
	if err != nil {
		writeMapped(w, err)
		return
	}

	out := ForkResponse{Forks: make([]ForkEntry, 0, len(results))}
	made := 0
	for _, res := range results {
		entry := ForkEntry{}
		switch {
		case res.Err != nil:
			entry.Error = res.Err.Error()
		case res.Machine != nil:
			m := d.toAPI(r.Context(), *res.Machine, actingOrg(r),
				d.startOf(r.Context(), res.Machine.ID), nil, URLAuthPublic)
			entry.Machine = &m
			made++
		}
		out.Forks = append(out.Forks, entry)
	}
	// 201 when anything was made, even if not everything: the caller has
	// machines and needs to know which. 207 would be more precise and is not
	// worth a status most clients treat as an error.
	status := http.StatusCreated
	if made == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, out)
}

// MaxForkCount mirrors the manager's cap, so the refusal happens at the edge
// with a message about the request rather than inside a lifecycle path.
const MaxForkCount = 100

// ForkOptions is what the manager is asked for. Separate from ForkRequest
// because the wire shape carries no org and no source: the API resolves both,
// and a body that could name either would be a body that could name somebody
// else's machine.
type ForkOptions struct {
	Machine    string
	Checkpoint string
	Name       string
	Count      int
	Volume     bool
	OrgID      string
}

// ForkOutcome is one fork's result as the manager reports it.
type ForkOutcome struct {
	Machine *state.Machine
	Err     error
}
