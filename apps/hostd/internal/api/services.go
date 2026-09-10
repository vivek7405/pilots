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
func (d Deps) serviceToAPI(svc state.Service, orgID string) Service {
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
		out.URL = d.URL.Of(svc.Domain + "." + d.Domain)
	}
	return out
}

// volumeOf is the volume a service mounts, empty when it mounts none.
//
// Separate from serviceToAPI so that stays a pure conversion; a list reads the
// bindings once and joins in memory rather than querying per row.
//
// A store error is returned rather than read as "no volume". On the patch
// route that answer would open the side door the single-mounter rule closes:
// a service that mounts a volume would take replicas: 2 because one read
// blipped.
func (d Deps) volumeOf(ctx context.Context, serviceID string) (string, error) {
	sv, err := d.Store.ServiceVolume(ctx, serviceID)
	if errors.Is(err, state.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sv.VolumeID, nil
}

func (d Deps) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var req CreateServiceRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	if req.Name == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "name is required", "pass name", nil)
		return
	}
	// Two ways of saying opposite things about the same field. Refused rather
	// than resolved by precedence, because either guess silently gives the
	// caller a service that is not the one they asked for.
	if req.URLAuth != "" && req.URLAuth != URLAuthPublic && req.URLAuth != URLAuthOrg {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "url_auth must be public or org", "pass url_auth: public, or url_auth: org", nil)
		return
	}
	if !checkLabels(w, req.Labels) {
		return
	}
	if req.Private && req.Domain != "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"private and domain contradict: a private service has no address",
			"drop one of them", nil)
		return
	}

	// A service nothing can ever wake is refused rather than silently
	// redefined as "stopped". No domain means no request can route to it and
	// no app means no peer can resolve it by name to wake it either -- with
	// zero replicas it would sit there costing nothing and doing nothing, and
	// become a support ticket six months later.
	//
	// Reachable now only with private: true, since every other service is
	// given an address below. That is exactly the service this describes, so
	// the rule and its message are unchanged.
	if req.Replicas == 0 && req.Domain == "" && req.App == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "a service with "+
			"no domain, no app and no running replicas can never be reached or "+
			"woken: give it a domain to route to, an app so peers can resolve it "+
			"by name, or at least one replica",
			"give it a domain, an app, or at least one replica", nil)
		return
	}

	// A volume is another tenant's data, and it is mounted by exactly one
	// machine at a time -- so the four things that could make this create
	// wrong are refused here rather than discovered at the claim, minutes
	// into the first deploy.
	//
	// Tenancy and the replica count are decided here for good. The last two --
	// the volume already attached to a machine, and the volume already bound
	// to another service -- are a fast, friendly refusal and NOT the
	// enforcement: the read is local and the binding is written a few dozen
	// lines below, and two creates naming one volume on two hosts arbitrate
	// two different service ids, so both scans pass and both bindings are
	// written. claimVolume (machines/volumes.go) is what actually prevents two
	// mounters, because the volume row has one writer and the first deploy to
	// claim it wins. What this saves is the common case: one operator, one
	// mistake, told immediately instead of minutes into a build.
	var volume *state.Volume
	if req.Volume != "" {
		v, ok := d.ownedVolume(w, r, req.Volume)
		if !ok {
			return // 404 on unknown and on foreign alike; existence never leaks
		}
		if req.Replicas > 1 {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "a service that "+
				"mounts a volume runs exactly one replica: a volume is mounted by "+
				"one machine at a time",
				"a volume-backed service runs one replica; drop replicas or the volume", nil)
			return
		}
		if v.MachineID != "" {
			WriteError(w, http.StatusConflict, CodeVolumeInUse, fmt.Sprintf(
				"volume %s is attached to machine %s; destroying it releases the volume",
				v.ID, v.MachineID), "destroy machine "+v.MachineID, nil)
			return
		}
		bindings, err := d.Store.ListServiceVolumes(r.Context())
		if err != nil {
			writeMapped(w, err)
			return
		}
		for _, b := range bindings {
			if b.VolumeID == v.ID {
				WriteError(w, http.StatusConflict, CodeVolumeInUse, fmt.Sprintf(
					"volume %s is already mounted by service %s", v.ID, b.ServiceID),
					"detach it from service "+b.ServiceID+" first", nil)
				return
			}
		}
		volume = v
	}

	// Connecting a service to a repository is the PUSH half of the same
	// authorization question POST /v1/builds asks, and it is asked here with
	// the same function. A push is a signed delivery resolved to the service
	// rows that name a repository (internal/github, serviceFor), so a service
	// naming someone else's repository is a standing order to build their
	// source into a machine of ours on their next commit -- the very hole the
	// {repo, ref} gate was closed for, reached the long way round.
	//
	// The SHAPE is checked first, and it matters more here than it reads: this
	// string is now an authorization key (it keys the repo_links row the check
	// below reads), it is reflected verbatim into that check's refusal, and on
	// success it is stored for serviceFor to match GitHub deliveries against.
	// Every other route that takes a repository runs RepoSlug for the reason
	// on RepoSlug itself; this one took an arbitrary megabyte.
	if req.Repo != "" {
		if !RepoSlug.MatchString(req.Repo) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "repo must be owner/name",
				`send {"repo":"owner/name"}`, nil)
			return
		}
		if !AllowRepo(w, r, d.Store, req.Repo) {
			return
		}
	}

	// A service's replicas are machines, so a create is admitted against the
	// same limits a create of that many machines would be. A replica boots
	// with the manager's defaults, which is where these numbers come from.
	req.OrgID = actingOrg(r)
	if !d.checkQuota(w, r, quota.Delta{
		Machines: req.Replicas, VCPUs: req.Replicas, MemMiB: req.Replicas * 512,
	}) {
		return
	}

	// The address, decided here and never again: at create for every service
	// that is not private, and otherwise only through a patch on a service
	// that has none. Nothing in the deploy path allocates, so a rollout adds
	// no write and no read to what is already the hot path.
	//
	// After the quota check so a refused create allocates nothing, and before
	// the row is built so a refused label writes nothing.
	if !req.Private {
		label, aerr := d.allocateLabel(r.Context(), req.Name, req.Domain)
		if aerr != nil {
			aerr.write(w)
			return
		}
		req.Domain = label
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
			WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
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
			WriteError(w, http.StatusBadRequest, CodeNotConfigured, "this host has "+
				"no fleet key, so it cannot store secrets; set PILOT_FLEET_KEY",
				"set PILOT_FLEET_KEY on every host, or send env instead of secret_env", nil)
			return
		}
		raw, err := json.Marshal(req.SecretEnv)
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
			return
		}
		sealed, err := d.FleetKey.Seal(raw)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal, "sealing the environment: "+err.Error(),
				NextInternal, nil)
			return
		}
		svc.EnvSealed = sealed
	}

	// Tenancy first, so a create that dies between the two writes leaves a
	// row nothing points at rather than a service no org owns.
	if err := d.Store.PutTenancy(r.Context(), &state.Tenancy{
		ID: svc.ID, OrgID: req.OrgID, Kind: "service", CreatedAt: svc.CreatedAt,
	}); err != nil {
		writeMapped(w, err)
		return
	}
	// Before the service row, for the reason tenancy is: a create that dies
	// between the two leaves a binding that refuses the volume loudly, not a
	// service that exists and deploys without it quietly.
	if volume != nil {
		if err := d.Store.PutServiceVolume(r.Context(), &state.ServiceVolume{
			ServiceID: svc.ID, Ordinal: 1, VolumeID: volume.ID, CreatedAt: svc.CreatedAt,
		}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	if err := d.Store.PutService(r.Context(), svc); err != nil {
		if volume != nil {
			// Best effort: the binding named a service that never appeared,
			// and leaving it would refuse this volume to every later create.
			_ = d.Store.DeleteServiceVolumes(r.Context(), svc.ID)
		}
		writeMapped(w, err)
		return
	}
	if len(req.Labels) > 0 {
		if err := d.Store.PutLabels(r.Context(), &state.Labels{ID: svc.ID, Kind: "service", Labels: req.Labels, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	if req.URLAuth == URLAuthOrg {
		if err := d.Store.PutURLAuth(r.Context(), &state.URLAuth{ID: svc.ID, Kind: "service", Mode: URLAuthOrg, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	out := d.serviceToAPI(*svc, req.OrgID)
	if volume != nil {
		out.VolumeID = volume.ID
	}
	// Echoed, the way the machine create echoes them: a client that reads
	// url_auth back from the create to confirm the service is gated would
	// otherwise be told "public" about a service that is not.
	out.Labels = req.Labels
	out.URLAuth = orDefaultMode(req.URLAuth)
	writeJSON(w, http.StatusCreated, out)
}

func (d Deps) handleListServices(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListServices(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	// One read of the bindings, joined in memory: a query per row would turn
	// a list into N of them against the local agent.
	bindings, err := d.Store.ListServiceVolumes(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	mounts := make(map[string]string, len(bindings))
	for _, b := range bindings {
		mounts[b.ServiceID] = b.VolumeID
	}

	// One pass over the same rows, grouped by owner and app, and the owner
	// each row resolved to comes back with the groups: the filter below reads
	// that map rather than asking the tenancy store again per row, so a list
	// costs one lookup per row, not two. See depends.go.
	groups, owners := d.siblingsOf(r.Context(), rows)

	org, narrow := listOrg(r)
	out := make([]Service, 0, len(rows))
	for _, svc := range rows {
		// The same rule `visible` applies, over the owners already resolved.
		owner, found := owners[svc.ID]
		if !visibleTo(owner, found, org, narrow) {
			continue
		}
		row := d.serviceToAPI(svc, owner)
		row.VolumeID = mounts[svc.ID]
		row.DependsOn = d.dependsOn(svc, groups[siblingKey{org: owner, app: svc.App}])
		row.Labels = d.labelsOf(r.Context(), svc.ID)
		row.URLAuth = d.urlAuthOf(r.Context(), svc.ID)
		out = append(out, row)
	}
	if want := labelFilter(r); len(want) > 0 {
		kept := out[:0]
		for _, s := range out {
			if matchesLabels(s.Labels, want) {
				kept = append(kept, s)
			}
		}
		out = kept
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.ownedService(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	volumeID, err := d.volumeOf(r.Context(), svc.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := d.serviceToAPI(*svc, owner)
	out.VolumeID = volumeID
	d.withEdges(r.Context(), &out, *svc, owner)
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
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	// Repointing a service at a repository is the same claim the create makes,
	// so it is checked the same way, shape included -- see handleCreateService
	// for why the shape is not cosmetic here. An empty string is the
	// dashboard's explicit disconnect and needs neither check: giving a
	// repository up is never something to be refused.
	if req.Repo != nil && *req.Repo != "" {
		if !RepoSlug.MatchString(*req.Repo) {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "repo must be owner/name",
				`send {"repo":"owner/name"}`, nil)
			return
		}
		if !AllowRepo(w, r, d.Store, *req.Repo) {
			return
		}
	}
	volumeID, err := d.volumeOf(r.Context(), svc.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	// An address is validated and claimed with the same reads a create uses,
	// which need the store, so it happens here rather than inside the pure
	// merge below. Only when the service has none: giving one that has an
	// address another is the 409 applyServicePatch returns.
	if req.Domain != nil && *req.Domain != "" && svc.Domain == "" {
		label, aerr := d.allocateLabel(r.Context(), svc.Name, *req.Domain)
		if aerr != nil {
			aerr.write(w)
			return
		}
		req.Domain = &label
	}

	before := svc.Replicas
	if err := d.applyServicePatch(svc, volumeID, req); err != nil {
		if errors.Is(err, errAddressSet) {
			WriteError(w, http.StatusConflict, CodeConflict, err.Error(),
				"add a custom domain instead", nil)
			return
		}
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	if req.URLAuth != nil {
		if *req.URLAuth != URLAuthPublic && *req.URLAuth != URLAuthOrg {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "url_auth must be public or org", "pass url_auth: public, or url_auth: org", nil)
			return
		}
		if err := d.Store.PutURLAuth(r.Context(), &state.URLAuth{ID: svc.ID, Kind: "service", Mode: *req.URLAuth, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
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
		writeMapped(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := d.serviceToAPI(*svc, owner)
	out.VolumeID = volumeID
	d.withEdges(r.Context(), &out, *svc, owner)
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
	if req.Domain != nil {
		switch {
		case *req.Domain == "":
			return errors.New("an address cannot be removed: URLs are permanent")
		case svc.Domain != "":
			return errAddressSet
		}
		// Validated and claimed by handleUpdateService before this runs, for
		// the same reason a create allocates before it builds the row: the
		// check needs the store and this function is a pure merge.
		svc.Domain = *req.Domain
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
		writeMapped(w, err)
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
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host cannot deploy: no object storage is configured",
			"deploy from a host with object storage; pilot status lists hosts", nil)
		return
	}
	var req DeployRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	if req.Build == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "build is required",
			"pass build: a rootfs build id from POST /v1/builds or pilot deploy", nil)
		return
	}
	// Validated HERE, because the rollout merges these partially onto what the
	// previous release's replicas carry and has no way to report a bad field
	// without discarding the good ones. Unvalidated, {"min_machines_running":
	// "one"} is a 200 whose replicas silently keep the old floor -- an
	// operator asking for a warm replica and being told it worked.
	if _, err := DecodeKnobs(req.Knobs); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(),
			"knobs are auto_stop (off or suspend), auto_start, min_machines_running, soft_limit, idle_timeout (1..3600 seconds)", nil)
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
		writeMapped(w, err)
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
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host cannot roll back: no object storage is configured",
			"roll back from a host with object storage; pilot status lists hosts", nil)
		return
	}
	rel, err := d.Rollout.Rollback(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, releaseToAPI(*rel))
}

func (d Deps) handlePromote(w http.ResponseWriter, r *http.Request) {
	if d.Rollout == nil {
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host cannot promote: no object storage is configured",
			"promote from a host with object storage; pilot status lists hosts", nil)
		return
	}
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req PromoteRequest
	if r.ContentLength > 0 {
		if err := decodeBody(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
			return
		}
	}
	if row.VolumeID != "" {
		if req.Replicas > 1 {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf(
				"machine %s mounts volume %s, so the service it becomes runs exactly "+
					"one replica: a volume is mounted by one machine at a time",
				row.ID, row.VolumeID),
				"a volume-backed service runs one replica; drop replicas or the volume", nil)
			return
		}
		// A volume-backed service is redeployed and rolled back by BOOTING
		// its one machine from an image, never by restoring a checkpoint that
		// carries the volume drive in its device state. A template sandbox
		// has no image for that boot to use.
		if row.ImageRef == "" {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, fmt.Sprintf(
				"machine %s mounts a volume and was created from the template, not "+
					"from an image; a volume-backed service is redeployed from its "+
					"image, so create the sandbox with image to promote it", row.ID),
				"create the sandbox with image, then promote it", nil)
			return
		}
	}
	svc, err := d.Rollout.Promote(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeMapped(w, err)
		return
	}
	// The machine's labels are the service's now: promote changes the
	// lifecycle, not the identity, and a label is how a caller finds it.
	if l := d.labelsOf(r.Context(), r.PathValue("id")); len(l) > 0 {
		if err := d.Store.PutLabels(r.Context(), &state.Labels{ID: svc.ID, Kind: "service", Labels: l, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	// And so is who may reach its URL. The URL does not change across a
	// promote, so neither may the answer to "who may reach it": without this
	// an org-gated sandbox becomes a public service the moment it is
	// promoted, and every replica the service gains afterwards -- which
	// carries no mode of its own -- would be reachable by anyone.
	if mode := d.urlAuthOf(r.Context(), r.PathValue("id")); mode == URLAuthOrg {
		if err := d.Store.PutURLAuth(r.Context(), &state.URLAuth{ID: svc.ID, Kind: "service", Mode: mode, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	volumeID, verr := d.volumeOf(r.Context(), svc.ID)
	if verr != nil {
		writeMapped(w, verr)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), svc.ID)
	out := d.serviceToAPI(*svc, owner)
	out.VolumeID = volumeID
	d.withEdges(r.Context(), &out, *svc, owner)
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

// newServiceID mints an id this host is allowed to write. See state.NewOwnedID.
func (d Deps) newServiceID(ctx context.Context) string {
	hosts, err := d.Store.ListHosts(ctx)
	if err != nil {
		return "svc_" + uuid.NewString()
	}
	return state.NewOwnedID("svc_", d.HostID, state.LiveHosts(hosts))
}
