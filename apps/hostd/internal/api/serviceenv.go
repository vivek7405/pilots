package api

import (
	"encoding/json"
	"net/http"
)

// Reading a service's environment back, values and all.
//
// # Why this route exists at all, when every other route hides values
//
// `GET /v1/services/{id}` returns variable NAMES and never values, and that is
// the right default: a list, a panel and a log line all want to say what is set
// without saying what it is. But somewhere the values have to be readable, or
// the platform is one where a password can be written and never recovered --
// which does not protect anybody, it just means the operator keeps a second
// copy somewhere worse.
//
// So there is exactly ONE route that answers with values, it is a deliberate
// act to call it, and it is this one.
//
// # What guards it
//
// The same ownership check every write on a service passes: an org that does
// not own the service gets the same 404 it gets for a service that does not
// exist, so the route cannot be used to discover that one exists. Sealed
// values are opened with the fleet key, which lives on the host and never in
// Corrosion, so a Corrosion replica read on any host yields ciphertext.
//
// The route sits under /v1/services, so it needs a DEPLOY-scoped key. That is
// the right level rather than an administrative one: a deploy key already sets
// these values and already deploys code that could print them, so reading them
// back grants nothing it did not have. A machines-scoped key reaches none of
// this, which is the boundary that matters.
//
// Nothing here is persisted, logged or cached. The values exist in this
// process for the length of one response.

// ServiceEnvResponse is a service's environment with the values in it.
//
// Two maps rather than one, because the difference survives the round trip: Env
// was written in the clear and SecretEnv was sealed, and a client that merged
// them would lose the ability to write them back the way they came.
type ServiceEnvResponse struct {
	ServiceID string            `json:"service_id"`
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`
	// Sealed says whether this host could open the sealed half. False with a
	// non-empty service means the host has no fleet key, which is a
	// configuration problem rather than an empty environment -- and reporting
	// it as an empty environment is how somebody concludes their secrets were
	// lost.
	Sealed bool `json:"sealed"`
}

func (d Deps) handleServiceEnv(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	out := ServiceEnvResponse{ServiceID: svc.ID, Env: map[string]string{}}
	if svc.Env != "" {
		if err := json.Unmarshal([]byte(svc.Env), &out.Env); err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal,
				"this service's environment is not readable as a map",
				"set it again with `pilot env set`", nil)
			return
		}
	}
	if svc.EnvSealed == "" {
		// Nothing sealed is not the same as "could not open what was sealed",
		// so this reports success rather than leaving Sealed false and letting
		// the caller read it as a failure.
		out.Sealed = true
		writeJSON(w, http.StatusOK, out)
		return
	}
	if d.FleetKey == nil || !d.FleetKey.IsSet() {
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host holds no fleet key, so it cannot open this service's sealed values",
			"set PILOT_FLEET_KEY on every host; the names are still readable from the service itself", nil)
		return
	}
	raw, err := d.FleetKey.Open(svc.EnvSealed)
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host's fleet key does not open this service's sealed values",
			"the hosts disagree about PILOT_FLEET_KEY; make them match", nil)
		return
	}
	// Cleared as soon as it has been copied out, so the plaintext does not sit
	// in this process's heap waiting for a garbage collector.
	defer clear(raw)
	if err := json.Unmarshal(raw, &out.SecretEnv); err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal,
			"this service's sealed environment is not readable as a map",
			"set it again with `pilot env set`", nil)
		return
	}
	out.Sealed = true
	writeJSON(w, http.StatusOK, out)
}
