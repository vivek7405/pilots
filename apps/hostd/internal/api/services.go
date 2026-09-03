package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Rollout is the deploy surface. An interface so the API can be tested
// without a machine layer, and nil on a host that has no object storage --
// where a release has nowhere to come from.
type Rollout interface {
	Deploy(ctx context.Context, serviceID, rootfsBuildID string) (*state.Release, error)
	Rollback(ctx context.Context, serviceID string) (*state.Release, error)
	Promote(ctx context.Context, machineID string, req PromoteRequest) (*state.Service, error)
}

// serviceToAPI never returns env or env_sealed. The sealed blob is not a
// secret to the fleet but it is not the client's either, and the plaintext
// half has no business on a list endpoint.
func serviceToAPI(svc state.Service, domain, orgID string) Service {
	out := Service{
		ID: svc.ID, Name: svc.Name, OrgID: orgID, App: svc.App, ReleaseID: svc.ReleaseID,
		Replicas: svc.Replicas, CustomDomain: svc.CustomDomain,
		Repo: svc.Repo, Branch: svc.Branch, Autodeploy: svc.Autodeploy,
		CreatedAt: svc.CreatedAt,
	}
	if svc.Health != "" {
		var h HealthCheck
		if json.Unmarshal([]byte(svc.Health), &h) == nil {
			out.Health = &h
		}
	}
	if svc.Domain != "" {
		out.URL = "https://" + svc.Domain + "." + domain
	}
	return out
}

func (d Deps) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var req CreateServiceRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "name is required"})
		return
	}

	// A service nothing can ever wake is refused rather than silently
	// redefined as "stopped". No domain means no request can route to it and
	// no app means no peer can resolve it by name to wake it either -- with
	// zero replicas it would sit there costing nothing and doing nothing, and
	// become a support ticket six months later.
	if req.Replicas == 0 && req.Domain == "" && req.App == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "a service with " +
			"no domain, no app and no running replicas can never be reached or " +
			"woken: give it a domain to route to, an app so peers can resolve it " +
			"by name, or at least one replica"})
		return
	}

	// A service's replicas are machines, so a create is admitted against the
	// same limits a create of that many machines would be. A replica boots
	// with the manager's defaults, which is where these numbers come from.
	req.OrgID = OrgID(r.Context())
	if !d.checkQuota(w, r, quota.Delta{
		Machines: req.Replicas, VCPUs: req.Replicas, MemMiB: req.Replicas * 512,
	}) {
		return
	}

	svc := &state.Service{
		// An id this host arbitrates, so the create is not refused by the
		// guard that protects every later write. See services.NewServiceID.
		ID: d.newServiceID(r.Context()), Name: req.Name, App: req.App,
		Replicas: req.Replicas, Domain: req.Domain, CustomDomain: req.CustomDomain,
		Repo: req.Repo, Branch: req.Branch, Autodeploy: req.Autodeploy,
		CreatedAt: time.Now().Unix(),
	}
	if req.Health != nil {
		raw, _ := json.Marshal(req.Health)
		svc.Health = string(raw)
	}
	if len(req.Env) > 0 {
		raw, err := json.Marshal(req.Env)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		svc.Env = string(raw)
	}

	// Sealed HERE, never dropped. This path used to marshal Env and ignore
	// SecretEnv entirely: a create carrying secrets answered 201, stored
	// nothing, and deployed a service whose secrets were simply absent, with
	// nothing anywhere reporting it. The machine path has always sealed
	// (machines/env.go); the service path did not.
	if len(req.SecretEnv) > 0 {
		if d.FleetKey == nil || !d.FleetKey.IsSet() {
			// Refused rather than degraded, for the same reason the machine
			// path refuses: writing these in the clear replicates them to
			// every host and into every backup, and nothing downstream would
			// report that it had happened.
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "this host has " +
				"no fleet key, so it cannot store secrets; set PILOT_FLEET_KEY"})
			return
		}
		raw, err := json.Marshal(req.SecretEnv)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
		sealed, err := d.FleetKey.Seal(raw)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				ErrorResponse{Error: "sealing the environment: " + err.Error()})
			return
		}
		svc.EnvSealed = sealed
	}

	// Tenancy first, so a create that dies between the two writes leaves a
	// row nothing points at rather than a service no org owns.
	if err := d.Store.PutTenancy(r.Context(), &state.Tenancy{
		ID: svc.ID, OrgID: req.OrgID, Kind: "service", CreatedAt: svc.CreatedAt,
	}); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := d.Store.PutService(r.Context(), svc); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, serviceToAPI(*svc, d.Domain, req.OrgID))
}

func (d Deps) handleListServices(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListServices(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	org, narrow := listOrg(r)
	out := make([]Service, 0, len(rows))
	for _, svc := range rows {
		owner, ok := d.visible(r, svc.ID, org, narrow)
		if !ok {
			continue
		}
		out = append(out, serviceToAPI(svc, d.Domain, owner))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	writeJSON(w, http.StatusOK, serviceToAPI(*svc, d.Domain, owner))
}

func (d Deps) handleDeploy(w http.ResponseWriter, r *http.Request) {
	// Ownership before forwarding: a foreign service must not be told which
	// host arbitrates it, and must not be acted on anywhere.
	if _, ok := d.ownedService(w, r, r.PathValue("id")); !ok {
		return
	}
	// Only the arbiter may write this service; forward rather than refuse.
	if d.forwardToArbiter(w, r, r.PathValue("id")) {
		return
	}
	if d.Rollout == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			ErrorResponse{Error: "this host cannot deploy: no object storage is configured"})
		return
	}
	var req DeployRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if req.Build == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "build is required"})
		return
	}
	// A rollout boots one extra machine before it retires the old one, so a
	// deploy is admitted against one replica's worth of headroom.
	if !d.checkQuota(w, r, quota.Delta{Machines: 1, VCPUs: 1, MemMiB: 512}) {
		return
	}
	rel, err := d.Rollout.Deploy(r.Context(), r.PathValue("id"), req.Build)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseToAPI(*rel))
}

func (d Deps) handleRollback(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedService(w, r, r.PathValue("id")); !ok {
		return
	}
	// Only the arbiter may write this service; forward rather than refuse.
	if d.forwardToArbiter(w, r, r.PathValue("id")) {
		return
	}
	if d.Rollout == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			ErrorResponse{Error: "this host cannot roll back: no object storage is configured"})
		return
	}
	rel, err := d.Rollout.Rollback(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseToAPI(*rel))
}

func (d Deps) handlePromote(w http.ResponseWriter, r *http.Request) {
	if d.Rollout == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			ErrorResponse{Error: "this host cannot promote: no object storage is configured"})
		return
	}
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	var req PromoteRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
	}
	svc, err := d.Rollout.Promote(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	writeJSON(w, http.StatusOK, serviceToAPI(*svc, d.Domain, owner))
}

type Release struct {
	ID            string `json:"id"`
	ServiceID     string `json:"service_id"`
	RootfsBuildID string `json:"rootfs_build_id,omitempty"`
	MemBuildID    string `json:"mem_build_id,omitempty"`
	Healthy       bool   `json:"healthy"`
	CreatedAt     int64  `json:"created_at"`
}

func releaseToAPI(r state.Release) Release {
	return Release{
		ID: r.ID, ServiceID: r.ServiceID, RootfsBuildID: r.RootfsBuildID,
		MemBuildID: r.MemBuildID, Healthy: r.Healthy, CreatedAt: r.CreatedAt,
	}
}

// writeStoreError maps the store's sentinels onto status codes, so a caller
// can tell "you are not the writer" from "it does not exist" from "it broke".
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, state.ErrNotFound):
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
	case errors.Is(err, state.ErrNotOwner):
		// 409, not 403: the caller is allowed, it just lost a race or asked
		// the wrong host. Both are retryable against the right one.
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}

// newServiceID mints an id this host is allowed to write. See state.NewOwnedID.
func (d Deps) newServiceID(ctx context.Context) string {
	hosts, err := d.Store.ListHosts(ctx)
	if err != nil {
		return "svc_" + uuid.NewString()
	}
	return state.NewOwnedID("svc_", d.HostID, state.LiveHosts(hosts))
}
