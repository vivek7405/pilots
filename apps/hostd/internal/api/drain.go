package api

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Emptying a host on purpose, from the outside.
//
// Any host serves these routes, because every host serves the whole API. The
// named host performs its own drain: it is the only one that can, since it
// holds the machines and it is the only host allowed to offer them. A request
// that arrives elsewhere is forwarded there, the same shape a service write
// takes to its arbiter.

// Drainer is the host-local half: what THIS host does when it is the one being
// drained. An interface so the API can be tested without a machine layer.
type Drainer interface {
	// Drain moves every machine off this host and reports what happened.
	Drain(ctx context.Context, pick func(state.Machine) (string, bool)) (*DrainReport, error)
	// Undrain lets this host take machines again. Nothing moves back:
	// machines are where they are, and moving them again for tidiness would
	// be a second outage for no benefit.
	Undrain()
	// Draining reports the flag.
	Draining() bool
	// Take accepts a machine another host has offered.
	Take(ctx context.Context, machineID, handoffID string) error
}

// DrainReport is what a drain did.
type DrainReport struct {
	// Moved are the machines now on another host. Their ids, names and URLs
	// are unchanged: that is what makes a drain invisible to their users.
	Moved []string `json:"moved"`
	// Left are the machines still here, each with the reason. A machine no
	// host would take is a fleet-capacity problem, and it is reported rather
	// than retried for ever.
	Left   []string          `json:"left,omitempty"`
	Errors map[string]string `json:"errors,omitempty"`
	// Draining stays true after a drain that left something behind, so the
	// host keeps refusing new work until an operator says otherwise.
	Draining bool  `json:"draining"`
	Started  int64 `json:"started"`
}

// TakeRequest is one host telling another to take a machine it has offered.
//
// Internal: it arrives only over the mesh, carrying the forwarding marker, and
// the offer row is what actually authorises the move. This request just saves
// the target from waiting to notice.
type TakeRequest struct {
	HandoffID string `json:"handoff_id"`
}

func (d Deps) handleDrain(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r, "draining a host") {
		return
	}
	if d.forwardToHost(w, r, r.PathValue("id")) {
		return
	}
	if d.Drain == nil {
		WriteError(w, http.StatusNotImplemented, CodeNotImplemented,
			"this host cannot drain", "upgrade the host", nil)
		return
	}

	report, err := d.Drain.Drain(r.Context(), d.drainTarget(r.Context()))
	if err != nil {
		writeMapped(w, err)
		return
	}
	report.Draining = d.Drain.Draining()
	writeJSON(w, http.StatusOK, report)
}

func (d Deps) handleDrainStatus(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r, "draining a host") {
		return
	}
	if d.forwardToHost(w, r, r.PathValue("id")) {
		return
	}
	if d.Drain == nil {
		writeJSON(w, http.StatusOK, DrainReport{Draining: false})
		return
	}
	// What is still here, so an operator can watch a drain converge rather
	// than guess.
	left := []string{}
	if rows, err := d.Store.ListMachines(r.Context()); err == nil {
		for _, row := range rows {
			if row.HostID == r.PathValue("id") && row.State != state.StateDestroyed {
				left = append(left, row.ID)
			}
		}
	}
	writeJSON(w, http.StatusOK, DrainReport{Draining: d.Drain.Draining(), Left: left})
}

func (d Deps) handleUndrain(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r, "draining a host") {
		return
	}
	if d.forwardToHost(w, r, r.PathValue("id")) {
		return
	}
	if d.Drain != nil {
		d.Drain.Undrain()
	}
	writeJSON(w, http.StatusOK, DrainReport{Draining: false})
}

// handleTake is the internal route a draining host calls on its target.
//
// Authorised by the OFFER, not by this call: the store checks who offered the
// machine, to whom, whether the offer is the newest, and whether the machine is
// actually down. A forged call therefore moves nothing.
func (d Deps) handleTake(w http.ResponseWriter, r *http.Request) {
	if d.Drain == nil {
		WriteError(w, http.StatusNotImplemented, CodeNotImplemented,
			"this host cannot take machines", "upgrade the host", nil)
		return
	}
	var req TakeRequest
	if err := decodeBody(r, &req); err != nil || req.HandoffID == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"handoff_id is required", "pass the handoff row's id", nil)
		return
	}
	if err := d.Drain.Take(r.Context(), r.PathValue("id"), req.HandoffID); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// drainTarget picks where each machine goes, using the same ranking a create
// uses.
//
// The SAME function on purpose. A drain that placed machines by a different
// rule from a create would leave a freshly drained fleet unbalanced in a way
// the placer would then have to undo.
func (d Deps) drainTarget(ctx context.Context) func(state.Machine) (string, bool) {
	tried := map[string]map[string]bool{}

	return func(row state.Machine) (string, bool) {
		if tried[row.ID] == nil {
			tried[row.ID] = map[string]bool{}
		}
		// This host is excluded by definition: it is the one being emptied.
		exclude := []string{d.HostID}
		for id := range tried[row.ID] {
			exclude = append(exclude, id)
		}

		ranked := d.rankForCreate(ctx, CreateMachineRequest{
			Name: row.Name, VCPUs: row.VCPUs, MemMiB: row.MemMiB,
			MemBuildID: row.MemBuildID, RootfsBuildID: row.RootfsBuildID,
		}, exclude)
		for _, id := range ranked {
			if id == d.HostID || tried[row.ID][id] {
				continue
			}
			tried[row.ID][id] = true
			return id, true
		}
		return "", false
	}
}

// forwardToHost sends a host-scoped request to the host it names.
//
// A drain is performed BY the host being drained: it holds the machines, and
// it is the only host allowed to offer them. Refusing on every other host
// would make an operator responsible for reaching the right one, so the host
// that received it forwards instead -- exactly what a service write does.
func (d Deps) forwardToHost(w http.ResponseWriter, r *http.Request, hostID string) bool {
	if hostID == "" || hostID == d.HostID {
		return false
	}
	if r.Header.Get(forwardedHeader) != "" {
		// One hop, for the reason every other forward is one hop.
		return false
	}
	if d.Peers == nil {
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"this host cannot reach "+hostID,
			"run the command against "+hostID+" directly", nil)
		return true
	}
	addr, ok := d.Peers.InternalAddr(hostID)
	if !ok {
		WriteError(w, http.StatusNotFound, CodeNotFound,
			"no host "+hostID+" in this fleet", NextNotFound, nil)
		return true
	}
	target := &url.URL{Scheme: "http", Host: addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		out.Header.Set(forwardedHeader, d.HostID)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"host "+hostID+" is unreachable: "+err.Error(),
			"check that it is up; a host that is genuinely gone is self-healed rather than drained", nil)
	}
	proxy.ServeHTTP(w, r)
	return true
}

// adminOnly refuses a request that does not carry the admin scope.
//
// Draining is an operator action on the FLEET rather than on one tenant's
// objects: it moves every org's machines at once. A tenant-scoped key has no
// business asking for it, and the refusal says which scope is missing rather
// than pretending the route does not exist.
func adminOnly(w http.ResponseWriter, r *http.Request, what string) bool {
	if IsAdmin(r.Context()) {
		return true
	}
	WriteError(w, http.StatusForbidden, CodeScopeRequired,
		what+" needs an admin-scoped key",
		"ask an operator, or use a key with scope admin", nil)
	return false
}
