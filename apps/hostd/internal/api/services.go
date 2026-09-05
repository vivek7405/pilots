package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Deploy(ctx context.Context, serviceID, rootfsBuildID string, knobs json.RawMessage) (*state.Release, error)
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

// volumeOf is the volume a service mounts, or empty.
//
// Separate from serviceToAPI so that stays a pure conversion; a list reads the
// bindings once and joins in memory rather than querying per row.
func (d Deps) volumeOf(ctx context.Context, serviceID string) string {
	sv, err := d.Store.ServiceVolume(ctx, serviceID)
	if err != nil || sv == nil {
		return ""
	}
	return sv.VolumeID
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

	// A volume is another tenant's data, and it is mounted by exactly one
	// machine at a time -- so the four things that could make this create
	// wrong are refused here rather than discovered at the claim, minutes
	// into the first deploy.
	var volume *state.Volume
	if req.Volume != "" {
		v, ok := d.ownedVolume(w, r, req.Volume)
		if !ok {
			return // 404 on unknown and on foreign alike; existence never leaks
		}
		if req.Replicas > 1 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "a service that " +
				"mounts a volume runs exactly one replica: a volume is mounted by " +
				"one machine at a time"})
			return
		}
		if v.MachineID != "" {
			writeJSON(w, http.StatusConflict, ErrorResponse{Error: fmt.Sprintf(
				"volume %s is attached to machine %s; destroying it releases the volume",
				v.ID, v.MachineID)})
			return
		}
		bindings, err := d.Store.ListServiceVolumes(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for _, b := range bindings {
			if b.VolumeID == v.ID {
				writeJSON(w, http.StatusConflict, ErrorResponse{Error: fmt.Sprintf(
					"volume %s is already mounted by service %s", v.ID, b.ServiceID)})
				return
			}
		}
		volume = v
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
	// Before the service row, for the reason tenancy is: a create that dies
	// between the two leaves a binding that refuses the volume loudly, not a
	// service that exists and deploys without it quietly.
	if volume != nil {
		if err := d.Store.PutServiceVolume(r.Context(), &state.ServiceVolume{
			ServiceID: svc.ID, Ordinal: 1, VolumeID: volume.ID, CreatedAt: svc.CreatedAt,
		}); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	if err := d.Store.PutService(r.Context(), svc); err != nil {
		if volume != nil {
			// Best effort: the binding named a service that never appeared,
			// and leaving it would refuse this volume to every later create.
			_ = d.Store.DeleteServiceVolumes(r.Context(), svc.ID)
		}
		writeStoreError(w, err)
		return
	}
	out := serviceToAPI(*svc, d.Domain, req.OrgID)
	if volume != nil {
		out.VolumeID = volume.ID
	}
	writeJSON(w, http.StatusCreated, out)
}

func (d Deps) handleListServices(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListServices(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	// One read of the bindings, joined in memory: a query per row would turn
	// a list into N of them against the local agent.
	bindings, err := d.Store.ListServiceVolumes(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	mounts := make(map[string]string, len(bindings))
	for _, b := range bindings {
		mounts[b.ServiceID] = b.VolumeID
	}

	org, narrow := listOrg(r)
	out := make([]Service, 0, len(rows))
	for _, svc := range rows {
		owner, ok := d.visible(r, svc.ID, org, narrow)
		if !ok {
			continue
		}
		row := serviceToAPI(svc, d.Domain, owner)
		row.VolumeID = mounts[svc.ID]
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := serviceToAPI(*svc, d.Domain, owner)
	out.VolumeID = d.volumeOf(r.Context(), svc.ID)
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleUpdateService(w http.ResponseWriter, r *http.Request) {
	// Ownership before forwarding: a foreign service must not be told which
	// host arbitrates it, and must not be acted on anywhere.
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// Only the arbiter may write this service; forward rather than refuse.
	if d.forwardToArbiter(w, r, r.PathValue("id")) {
		return
	}

	// Strict on THIS route alone, where decodeBody is lenient everywhere else.
	// The clients send knobs nowhere but here, and a knobs key silently
	// dropped is a compose file that says one thing and a service that does
	// another -- an operator asking for a warm replica, being told it worked,
	// and finding out from a cold start.
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	var req UpdateServiceRequest
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	volumeID := d.volumeOf(r.Context(), svc.ID)
	before := svc.Replicas
	if err := d.applyServicePatch(svc, volumeID, req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	// A replica is a machine, so a scale-up is admitted against the same
	// limits the create was admitted against. Nothing downstream would catch
	// it: the deploy admits ONE replica's headroom whatever the count says,
	// and the rollout creates machines through the manager rather than through
	// the API, so without this a service created at one replica could be
	// patched to a hundred and the next deploy would boot all hundred.
	if grew := svc.Replicas - before; grew > 0 {
		if !d.checkQuota(w, r, quota.Delta{
			Machines: grew, VCPUs: grew, MemMiB: grew * 512,
		}) {
			return
		}
	}
	if err := d.Store.PutService(r.Context(), svc); err != nil {
		writeStoreError(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := serviceToAPI(*svc, d.Domain, owner)
	out.VolumeID = volumeID
	writeJSON(w, http.StatusOK, out)
}

// applyServicePatch applies the present fields onto the row and validates the
// MERGED result, so a patch is judged by what the service will be rather than
// by what the body said.
//
// One function on purpose: every rule about a legal service lives here, and a
// new field is a case in this switch plus a line in UpdateServiceRequest.
func (d Deps) applyServicePatch(svc *state.Service, volumeID string, req UpdateServiceRequest) error {
	if req.Replicas != nil {
		if *req.Replicas < 0 {
			return errors.New("replicas cannot be negative")
		}
		svc.Replicas = *req.Replicas
	}
	if req.Health != nil {
		raw, err := json.Marshal(req.Health)
		if err != nil {
			return err
		}
		svc.Health = string(raw)
	}
	// A non-nil map REPLACES; an empty one clears. The client merges when it
	// wants a merge, because only the client knows which of the two it meant.
	if req.Env != nil {
		if len(req.Env) == 0 {
			svc.Env = ""
		} else {
			raw, err := json.Marshal(req.Env)
			if err != nil {
				return err
			}
			svc.Env = string(raw)
		}
	}
	if req.SecretEnv != nil {
		if len(req.SecretEnv) == 0 {
			svc.EnvSealed = ""
		} else {
			// Refused rather than degraded, exactly as the create path
			// refuses: writing these in the clear replicates them to every
			// host and into every backup, and nothing downstream reports it.
			if d.FleetKey == nil || !d.FleetKey.IsSet() {
				return errors.New("this host has no fleet key, so it cannot " +
					"store secrets; set PILOT_FLEET_KEY")
			}
			raw, err := json.Marshal(req.SecretEnv)
			if err != nil {
				return err
			}
			sealed, err := d.FleetKey.Seal(raw)
			if err != nil {
				return fmt.Errorf("sealing the environment: %w", err)
			}
			svc.EnvSealed = sealed
		}
	}
	if req.Repo != nil {
		svc.Repo = *req.Repo
	}
	if req.Branch != nil {
		svc.Branch = *req.Branch
	}
	if req.Autodeploy != nil {
		svc.Autodeploy = *req.Autodeploy
	}

	// The create-time rule, applied to the merged row. Scaling a routable
	// service to zero is fine; scaling one that nothing can reach or wake to
	// zero turns it into a support ticket six months later.
	if svc.Replicas == 0 && svc.Domain == "" && svc.App == "" {
		return errors.New("a service with no domain, no app and no running " +
			"replicas can never be reached or woken: give it a domain to route " +
			"to, an app so peers can resolve it by name, or at least one replica")
	}

	// The other create-time rule. A volume is mounted by one machine, so a
	// service that mounts one runs one replica; the create refused more and
	// the patch must not admit it by the side door.
	if volumeID != "" && svc.Replicas > 1 {
		return fmt.Errorf("service mounts volume %s and runs exactly one replica: "+
			"a volume is mounted by one machine at a time", volumeID)
	}
	return nil
}

// handleListReleases lists a service's release history, newest first.
func (d Deps) handleListReleases(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedService(w, r, r.PathValue("id")); !ok {
		return
	}
	// A read, so it is answered here rather than forwarded: every host serves
	// this from its own replica, which is the whole point of having one.
	rows, err := d.Store.ReleasesFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]Release, 0, len(rows))
	for _, rel := range rows {
		out = append(out, releaseToAPI(rel))
	}
	writeJSON(w, http.StatusOK, out)
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
	// Validated HERE, because the rollout merges these partially onto what the
	// previous release's replicas carry and has no way to report a bad field
	// without discarding the good ones. Unvalidated, {"min_machines_running":
	// "one"} is a 200 whose replicas silently keep the old floor -- an
	// operator asking for a warm replica and being told it worked.
	if _, err := DecodeKnobs(req.Knobs); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	// The build becomes this service's root filesystem, so it is scoped like
	// any other object the caller names by id.
	if !d.ownedBuild(w, r, req.Build) {
		return
	}
	// A rollout boots one extra machine before it retires the old one, so a
	// deploy is admitted against one replica's worth of headroom.
	if !d.checkQuota(w, r, quota.Delta{Machines: 1, VCPUs: 1, MemMiB: 512}) {
		return
	}
	rel, err := d.Rollout.Deploy(r.Context(), r.PathValue("id"), req.Build, req.Knobs)
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
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req PromoteRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
			return
		}
	}
	if row.VolumeID != "" {
		if req.Replicas > 1 {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf(
				"machine %s mounts volume %s, so the service it becomes runs exactly "+
					"one replica: a volume is mounted by one machine at a time",
				row.ID, row.VolumeID)})
			return
		}
		// A volume-backed service is redeployed and rolled back by BOOTING
		// its one machine from an image, never by restoring a checkpoint that
		// carries the volume drive in its device state. A template sandbox
		// has no image for that boot to use.
		if row.ImageRef == "" {
			writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf(
				"machine %s mounts a volume and was created from the template, not "+
					"from an image; a volume-backed service is redeployed from its "+
					"image, so create the sandbox with image to promote it", row.ID)})
			return
		}
	}
	svc, err := d.Rollout.Promote(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := serviceToAPI(*svc, d.Domain, owner)
	out.VolumeID = d.volumeOf(r.Context(), svc.ID)
	writeJSON(w, http.StatusOK, out)
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
