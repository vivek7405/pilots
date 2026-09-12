package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Granting a machine, or a service, the right to ask for credentials.
//
// # Why a grant is replace-only
//
// A PUT hands over the WHOLE grant, and there is no merge. Merging two partial
// grants produces a permission nobody wrote: somebody adds a scope, somebody
// else adds a secret, and what the machine ends up holding is the union neither
// of them reviewed. Replacing means the thing granted is always the thing
// somebody looked at.
//
// # Why a caller may only grant what it holds
//
// Otherwise the broker is a privilege escalator: a machines-scoped key that
// could grant `deploy` to a machine it controls has `deploy`, one round trip
// later. `admin` is refused outright at every layer -- here, at the mint, and
// again at the verify -- because it is the one scope whose escape is total.
//
// # Why values never come back
//
// GET answers with NAMES and scopes. A route that returned granted secrets
// would be a second reveal route with none of the deliberation the first one
// has, reachable by anybody who can already write the grant. What a machine is
// holding is a question the machine's own broker answers, to the machine.

// GrantRequest is what an operator grants. Both fields replace.
type GrantRequest struct {
	// Scopes a token may carry. Empty or absent means no token at all.
	Scopes []string `json:"scopes,omitempty"`
	// Secrets the machine may fetch. Empty or absent means none.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// GrantResponse is what is granted, without the values.
type GrantResponse struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	OrgID  string   `json:"org_id,omitempty"`
	Scopes []string `json:"scopes"`
	// SecretNames are the names granted, sorted. Never the values.
	SecretNames []string `json:"secret_names"`
	UpdatedAt   int64    `json:"updated_at,omitempty"`
}

func (d Deps) handleGetMachineGrant(w http.ResponseWriter, r *http.Request) {
	m, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	d.readGrant(w, r, m.ID, "machine")
}

func (d Deps) handlePutMachineGrant(w http.ResponseWriter, r *http.Request) {
	m, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// The OWNER writes it, because the row describes the machine and the host
	// that writes the machine's row is the one host allowed to write rows about
	// it. Forwarded rather than refused: a caller should not have to know which
	// host holds what.
	if d.forwardToHost(w, r, m.HostID) {
		return
	}
	d.writeGrant(w, r, m.ID, "machine")
}

func (d Deps) handleDeleteMachineGrant(w http.ResponseWriter, r *http.Request) {
	m, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToHost(w, r, m.HostID) {
		return
	}
	d.clearGrant(w, r, m.ID)
}

func (d Deps) handleGetServiceGrant(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	d.readGrant(w, r, svc.ID, "service")
}

func (d Deps) handlePutServiceGrant(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToArbiter(w, r, svc.ID) {
		return
	}
	d.writeGrant(w, r, svc.ID, "service")
}

func (d Deps) handleDeleteServiceGrant(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToArbiter(w, r, svc.ID) {
		return
	}
	d.clearGrant(w, r, svc.ID)
}

func (d Deps) readGrant(w http.ResponseWriter, r *http.Request, id, kind string) {
	grant, err := d.Store.GetBrokerGrant(r.Context(), id)
	if errors.Is(err, state.ErrNotFound) {
		// Nothing granted is a legitimate answer, not a 404: the object exists
		// and the answer to "what may it ask for" is "nothing". A 404 here
		// would make a client think it had the wrong id.
		writeJSON(w, http.StatusOK, GrantResponse{
			ID: id, Kind: kind, Scopes: []string{}, SecretNames: []string{},
		})
		return
	}
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{
		ID: grant.ID, Kind: grant.Kind, OrgID: grant.OrgID,
		Scopes:      grant.Scopes,
		SecretNames: grantedNames(d, r, grant),
		UpdatedAt:   grant.UpdatedAt,
	})
}

func (d Deps) writeGrant(w http.ResponseWriter, r *http.Request, id, kind string) {
	var req GrantRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}

	// A caller grants only what it holds. Without this the broker is a
	// privilege escalator: a machines-scoped key that could grant deploy has
	// deploy, one round trip later.
	held := rankOf(Scopes(r.Context()))
	scopes := make([]string, 0, len(req.Scopes))
	for _, scope := range req.Scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if scope == ScopeAdmin {
			WriteError(w, http.StatusForbidden, CodeScopeRequired,
				"admin is not a brokerable scope",
				"grant machines or deploy; a machine that needs admin needs an operator", nil)
			return
		}
		if !ValidScope(scope) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest,
				"no such scope: "+scope,
				"the scopes are machines and deploy", nil)
			return
		}
		if scopeRank[scope] > held {
			WriteError(w, http.StatusForbidden, CodeScopeRequired,
				"you cannot grant "+scope+", which you do not hold",
				"use a key with scope "+scope, nil)
			return
		}
		scopes = append(scopes, scope)
	}

	sealed := ""
	if len(req.Secrets) > 0 {
		if d.FleetKey == nil || !d.FleetKey.IsSet() {
			WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
				"this host holds no fleet key, so it cannot store a granted secret",
				"set PILOT_FLEET_KEY on every host, or grant scopes only", nil)
			return
		}
		raw, err := json.Marshal(req.Secrets)
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
			return
		}
		blob, err := d.FleetKey.Seal(raw)
		clear(raw)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal,
				"could not seal the granted secrets", NextInternal, nil)
			return
		}
		sealed = blob
	}

	org := actingOrg(r)
	if owner, known := d.tenancy().OrgOf(r.Context(), id); known {
		// The OBJECT's org, not the caller's. An admin key acting across orgs
		// must mint tokens for the org that owns the machine, or a token would
		// carry an org the machine does not belong to and every read through it
		// would be narrowed to the wrong tenant.
		org = owner
	}

	grant := &state.BrokerGrant{
		ID: id, Kind: kind, OrgID: org, Scopes: scopes, Sealed: sealed,
		UpdatedAt: time.Now().Unix(),
	}
	if err := d.Store.PutBrokerGrant(r.Context(), grant); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{
		ID: id, Kind: kind, OrgID: org, Scopes: scopes,
		SecretNames: sortedNames(req.Secrets), UpdatedAt: grant.UpdatedAt,
	})
}

func (d Deps) clearGrant(w http.ResponseWriter, r *http.Request, id string) {
	if err := d.Store.DeleteBrokerGrant(r.Context(), id); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grantedNames opens the sealed half only far enough to list its keys.
//
// A host with no key answers with no names rather than an error: the scopes are
// still worth seeing, and "this host cannot read the sealed half" is a
// configuration problem that the broker route reports where it matters.
func grantedNames(d Deps, r *http.Request, grant *state.BrokerGrant) []string {
	if grant.Sealed == "" || d.FleetKey == nil || !d.FleetKey.IsSet() {
		return []string{}
	}
	raw, err := d.FleetKey.Open(grant.Sealed)
	if err != nil {
		return []string{}
	}
	defer clear(raw)
	var secrets map[string]string
	if err := json.Unmarshal(raw, &secrets); err != nil {
		return []string{}
	}
	return sortedNames(secrets)
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
