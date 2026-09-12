package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Manager is the lifecycle surface the handlers drive. An interface rather
// than the concrete type so the API can be tested without booting VMs.
type Manager interface {
	// CollectMetrics folds the per-machine engine counters into the metrics
	// registry. Called on each scrape of GET /metrics, because the memory
	// handlers are separate processes that have to be asked.
	CollectMetrics()

	Create(ctx context.Context, req CreateMachineRequest) (*state.Machine, error)
	Destroy(ctx context.Context, id string) error
	Suspend(ctx context.Context, id string) error
	Wake(ctx context.Context, id string) error
	// Redeploy boots a machine again from another image, in place: same row,
	// same URL, same volume. How a volume-backed service takes a release.
	Redeploy(ctx context.Context, id string, req RedeployRequest) (*state.Machine, error)
	// Resize boots a machine again at a new size, in place: same row, same
	// URL, same disk, same volume. It is a boot rather than a restore because
	// a memory image cannot be loaded into a differently-sized VM, so the
	// machine loses what was in memory and nothing else.
	Resize(ctx context.Context, id string, vcpus, memMiB int) (*state.Machine, error)
	Checkpoint(ctx context.Context, machineID, comment string) (*state.Checkpoint, error)
	ListCheckpoints(ctx context.Context, machineID string) ([]state.Checkpoint, error)
	RestoreCheckpoint(ctx context.Context, checkpointID string) (*state.Machine, error)
	GetCheckpoint(ctx context.Context, checkpointID string) (*state.Checkpoint, error)
	Exec(ctx context.Context, machineID string, req ExecRequest) (*ExecResponse, error)
	Logs(ctx context.Context, machineID string) ([]byte, error)
	// Processes answers what a machine is running, as the agent's own JSON.
	// Passed through rather than re-encoded: the guest is the only thing that
	// knows whether a pid is alive, so a shape assembled on the host would be
	// a second copy of an answer that is stale the moment it is written.
	Processes(ctx context.Context, machineID string) ([]byte, error)
	// ProcessAction starts, stops or restarts one named process, leaving the
	// machine's other processes alone.
	ProcessAction(ctx context.Context, machineID, name, action string) error
	// ProcessLogs is one process's captured output, most recent last.
	ProcessLogs(ctx context.Context, machineID, name string, tail int) ([]byte, error)
	// ExecStream proxies the agent's websocket exec stream onto w. An error is
	// returned only before anything was written (a wake that failed, a machine
	// that is not running); once the upgrade has been attempted it is nil.
	ExecStream(w http.ResponseWriter, r *http.Request, machineID string) error
	// TCPStream carries one TCP connection to a port inside the machine.
	TCPStream(w http.ResponseWriter, r *http.Request, machineID string, port int) error
	// Terminal sessions, answered by the guest agent.
	SessionsJSON(ctx context.Context, machineID string) ([]byte, error)
	AttachStream(w http.ResponseWriter, r *http.Request, machineID, session string) error
	KillSession(ctx context.Context, machineID, session string) error
	// LogTail is Logs from a byte offset, for a follow. nil, nil when nothing
	// new has been written. It reads the file and nothing else: a follow polls
	// it twice a second, and whether the machine still exists is asked far
	// more rarely and separately.
	LogTail(machineID string, offset int64) ([]byte, error)
	CreateVolume(ctx context.Context, req CreateVolumeRequest) (*state.Volume, error)
	ListVolumes(ctx context.Context) ([]state.Volume, error)
	MachineVolume(ctx context.Context, machineID string) (*MachineVolume, error)
	// SnapshotVolume takes a point-in-time copy, pausing the guest for the
	// clone when one is running: a clone taken while the guest writes captures
	// a filesystem mid-update, which mounts and then fails later.
	SnapshotVolume(ctx context.Context, volumeID string) (string, error)
	ListVolumeSnapshots(ctx context.Context, volumeID string) ([]string, error)
	// RestoreVolumeSnapshot puts a snapshot back as the live image. Refuses a
	// running machine and drops a suspended one's memory image, which held
	// cached filesystem state from the disk being replaced.
	RestoreVolumeSnapshot(ctx context.Context, volumeID, stamp string) error
	// DeleteVolumeSnapshot removes one. This is what frees storage: a snapshot
	// holds a refcount on every block it references, so blocks the live volume
	// has overwritten stay until the snapshot goes.
	DeleteVolumeSnapshot(ctx context.Context, volumeID, stamp string) error
	// Fork makes new machines from one machine's or checkpoint's exact state:
	// new ids, new names, new URLs, the source's processes already running.
	Fork(ctx context.Context, opts ForkOptions) ([]ForkOutcome, error)
}

// toAPI converts a stored row to the wire shape.
//
// The URL is derived from the machine's domain rather than stored twice, so
// there is exactly one place a machine's address is decided.
//
// orgID is passed in rather than looked up here: a list endpoint already knows
// every row's owner from the pass it made to filter them, and re-asking per
// row would turn one lookup into N.
func (d Deps) toAPI(ctx context.Context, row state.Machine, orgID string, cpu state.MachineCPU, labels map[string]string, urlAuth string) Machine {
	parent, checkpoint := d.lineageOf(ctx, row.ID)
	return Machine{
		Labels:  labels,
		URLAuth: urlAuth,
		Egress:  d.egressOf(ctx, row.HostID, orgID),
		ID:      row.ID, Name: row.Name, HostID: row.HostID, State: row.State,
		OrgID:        orgID,
		Knobs:        ParseKnobs(row.KindKnobs),
		ImageRef:     row.ImageRef,
		VCPUs:        row.VCPUs,
		MemMiB:       row.MemMiB,
		URL:          d.URL.Of(row.Domain),
		CustomDomain: row.CustomDomain,
		VolumeID:     row.VolumeID,
		ServiceID:    row.ServiceID,
		ReleaseID:    row.ReleaseID,
		App:          row.App,
		CreatedAt:    row.UpdatedAt,
		LastActivity: row.LastActivity,
		LastStart:    cpu.LastStart,
		LastStartAt:  cpu.LastStartAt,
		Parent:       parent,
		Checkpoint:   checkpoint,
	}
}

// lineageOf is where a machine was forked from, or empty for one that was not.
//
// A store error reads as "not forked": the field is provenance, and a blipped
// side-table read must not fail a machine read.
func (d Deps) lineageOf(ctx context.Context, id string) (parent, checkpoint string) {
	l, err := d.Store.GetLineage(ctx, id)
	if err != nil || l == nil {
		return "", ""
	}
	return l.ParentID, l.CheckpointID
}

// urlAuthOf reads who may reach an object's URL; nothing recorded is public,
// which is what every URL was before the table existed.
func (d Deps) urlAuthOf(ctx context.Context, id string) string {
	u, err := d.Store.GetURLAuth(ctx, id)
	if err != nil || u == nil || u.Mode == "" {
		return URLAuthPublic
	}
	return u.Mode
}

func validURLAuth(mode string) bool {
	return mode == "" || mode == URLAuthPublic || mode == URLAuthOrg
}

// Label bounds. A labels map goes into a CRDT table replicated to every host
// and cached in each one's memory, so its size is fleet-wide cost rather than
// one row's: a caller that attached a megabyte of them (the body limit, and
// nothing below it said otherwise) would gossip that megabyte to the whole
// fleet for as long as the object lives.
const (
	maxLabels      = 32
	maxLabelKey    = 64
	maxLabelValue  = 256
	labelsTooLarge = "at most 32 labels, keys up to 64 bytes and values up to 256"
)

// checkLabels refuses a labels map that is outside those bounds. An empty key
// goes with them: `?label=k=v` cannot express one, so it could never be
// matched again.
func checkLabels(w http.ResponseWriter, labels map[string]string) bool {
	if len(labels) > maxLabels {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			fmt.Sprintf("%d labels is too many", len(labels)), labelsTooLarge, nil)
		return false
	}
	for k, v := range labels {
		if k == "" || len(k) > maxLabelKey || len(v) > maxLabelValue {
			WriteError(w, http.StatusBadRequest, CodeBadRequest,
				fmt.Sprintf("label %q is not within the limits", k), labelsTooLarge, nil)
			return false
		}
	}
	return true
}

func orDefaultMode(mode string) string {
	if mode == "" {
		return URLAuthPublic
	}
	return mode
}

// handleUpdateMachine is PATCH /v1/machines/{id}: the one thing a machine
// changes after create is who may reach its URL.
func (d Deps) handleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req UpdateMachineRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.URLAuth == nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "nothing to change", "pass url_auth: public, or url_auth: org", nil)
		return
	}
	if *req.URLAuth != URLAuthPublic && *req.URLAuth != URLAuthOrg {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "url_auth must be public or org", "pass url_auth: public, or url_auth: org", nil)
		return
	}
	if err := d.Store.PutURLAuth(r.Context(), &state.URLAuth{ID: row.ID, Kind: "machine", Mode: *req.URLAuth, UpdatedAt: time.Now().Unix()}); err != nil {
		writeMapped(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), row.ID)
	writeJSON(w, http.StatusOK, d.toAPI(r.Context(), *row, owner, d.startOf(r.Context(), row.ID), d.labelsOf(r.Context(), row.ID), *req.URLAuth))
}

// labelsOf reads an object's labels, and reads "none recorded" as none.
func (d Deps) labelsOf(ctx context.Context, id string) map[string]string {
	l, err := d.Store.GetLabels(ctx, id)
	if err != nil || l == nil {
		return nil
	}
	return l.Labels
}

// labelFilter is the ?label=k=v query, repeatable; every pair must match.
func labelFilter(r *http.Request) map[string]string {
	want := map[string]string{}
	for _, kv := range r.URL.Query()["label"] {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			want[k] = v
		}
	}
	return want
}

func matchesLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func toAPICheckpoint(c state.Checkpoint) Checkpoint {
	return Checkpoint{
		ID: c.ID, MachineID: c.MachineID, Seq: c.Seq, Comment: c.Comment,
		SourceID: c.SourceID, Durable: c.Durable, CreatedAt: c.CreatedAt,
		ResumeGapMS: c.ResumeGapMS,
	}
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// ErrConflict marks a lifecycle request the machine's current state forbids.
//
// The caller is allowed and the body is well formed; the machine simply
// cannot do this right now, and the message says what to do about it. Lives
// here rather than in machines because writeErr has to recognise it and
// machines imports this package, not the other way round.
var ErrConflict = errors.New("conflict")

// ErrNoCapacity marks a create this host cannot hold, even after suspending
// every idle machine it could.
//
// Not a failure and not the caller's fault: it is the fleet's honest answer to
// "I have nowhere to put this". The ranker reads it and offers the create to
// the next host; a client that has run out of hosts sees a 507 naming capacity
// rather than a 500 naming whatever Firecracker said when it could not get the
// memory.
//
// Lives here rather than in machines for the reason ErrConflict does: the
// error mapper has to recognise it, and machines imports this package.
var ErrNoCapacity = errors.New("no capacity")

func (d Deps) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	var req CreateMachineRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	// The org comes from the authenticated key, or from ?org= on an admin
	// key acting as another org, and overwrites whatever the body said.
	// CreateMachineRequest.OrgID is `json:"-"` so a body cannot carry one at
	// all, and this line is the only thing that ever sets it. See actingOrg.
	req.OrgID = actingOrg(r)

	// A create may name a volume to attach, and a volume is another tenant's
	// data. Without this, naming a foreign id in the body would mount someone
	// else's filesystem into a machine the caller controls -- the one place
	// tenancy could be crossed by a create rather than by a read.
	if req.Volume != "" {
		if _, ok := d.ownedVolume(w, r, req.Volume); !ok {
			return
		}
	}

	// And a create may name a built image, which is the same crossing by a
	// different door: the build id becomes this machine's root filesystem.
	if req.Image != "" {
		if !d.ownedBuild(w, r, req.Image) {
			return
		}
	}

	// The same door a third time. A release's build pair RESTORES another
	// org's memory image, and the fields decode from a body even though only
	// the rollout is meant to set them (see CreateMachineRequest). The
	// rollout never comes through this handler; it creates in-process. So a
	// pair named here is held to the check image gets: no memory build ever
	// has an owner row, which makes the pair admin-only, and a tenant naming
	// one is told the build is not there.
	for _, b := range []string{req.MemBuildID, req.RootfsBuildID} {
		if b != "" && !d.ownedBuild(w, r, b) {
			return
		}
	}

	// And a fourth time, for the door that hands over SECRETS rather than an
	// image. A create naming a service JOINS that service's row rather than
	// minting one (machines.Create), and a machine reads its service's sealed
	// environment back out at boot (machines.resolveEnv). Unchecked, a key
	// naming another org's service id got a machine of its own -- one it can
	// exec into -- with that service's decrypted secrets delivered inside it,
	// and a foreign replica in the victim's release set as well.
	if req.Service != "" {
		if _, ok := d.ownedService(w, r, req.Service); !ok {
			return
		}
	}

	// And the release id that travels beside it. A release has no tenancy row
	// of its own because it is owned THROUGH its service, so the check is that
	// the two agree. Every consumer matches the pair together -- a rollout
	// counts its replicas by service and release, and the idle sweep compares
	// a machine's release against its service's current one -- so a release
	// that does not belong to the service named here is at best inert and at
	// worst a replica counted into a rollout that never placed it.
	if req.Release != "" {
		rel, err := d.Store.GetRelease(r.Context(), req.Release)
		switch {
		case errors.Is(err, state.ErrNotFound):
			notFound(w, "release")
			return
		case err != nil:
			writeMapped(w, err)
			return
		case rel.ServiceID != req.Service:
			// The same answer as "no such release", deliberately: telling the
			// two apart would be a release-id oracle across tenants.
			notFound(w, "release")
			return
		}
	}

	if !validURLAuth(req.URLAuth) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "url_auth must be public or org", "pass url_auth: public, or url_auth: org", nil)
		return
	}
	if !checkPayloadSize(w, map[string]any{
		"env": req.Env, "secret_env": req.SecretEnv,
		"knobs": req.Knobs, "labels": req.Labels,
	}) {
		return
	}
	if !checkLabels(w, req.Labels) {
		return
	}
	// What a RESTRICTED key may make. Both checks are here rather than in the
	// manager because they are a property of the CALLER, not of the machine:
	// the rollout creates in-process with no key at all and must not be held
	// to an agent's consent screen. See limits.go.
	if !d.checkNamePrefix(w, r, &req.Name, "machine") {
		return
	}
	if !d.checkMachineCap(w, r) {
		return
	}
	if !d.checkQuota(w, r, quota.Delta{
		Machines: 1,
		VCPUs:    orDefault(req.VCPUs, 1),
		MemMiB:   orDefault(req.MemMiB, 512),
	}) {
		return
	}

	// AFTER the quota, because the org and the key are only resolved here and
	// a create the org may not have should be refused once, by the host that
	// received it, rather than forwarded somewhere to be refused again.
	//
	// Before the manager, because the whole point is that another host may be
	// the better place to run it. A volume-backed create is not forwarded: the
	// volume is claimed by whichever host mounts it, and moving the create
	// would move the claim without moving the data.
	if req.Volume == "" && d.forwardCreate(w, r, req) {
		return
	}

	row, err := d.Machines.Create(r.Context(), req)
	if err != nil {
		writeMapped(w, err)
		return
	}
	if len(req.Labels) > 0 {
		if err := d.Store.PutLabels(r.Context(), &state.Labels{ID: row.ID, Kind: "machine", Labels: req.Labels, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	if req.URLAuth == URLAuthOrg {
		if err := d.Store.PutURLAuth(r.Context(), &state.URLAuth{ID: row.ID, Kind: "machine", Mode: URLAuthOrg, UpdatedAt: time.Now().Unix()}); err != nil {
			writeMapped(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, d.toAPI(r.Context(), *row, req.OrgID, d.startOf(r.Context(), row.ID), req.Labels, orDefaultMode(req.URLAuth)))
}

// orDefault mirrors the machine manager's own defaulting, so the quota check
// counts the size the create will really ask for rather than the zero a client
// left out.
func orDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func (d Deps) handleListMachines(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	org, narrow := listOrg(r)
	want := labelFilter(r)
	builders := includeBuilders(r)
	out := make([]Machine, 0, len(rows))
	for _, row := range rows {
		// A builder is infrastructure hostd made for itself, not something the
		// org created, so it is absent unless asked for: an agent listing
		// machines to pick one to exec into should not have to know to skip
		// it, and a list that shows it invites someone to destroy the thing
		// their next deploy needs. GET /v1/builders is where they are.
		if !builders && isBuilder(row.Name) {
			continue
		}
		owner, ok := d.visible(r, row.ID, org, narrow)
		if !ok {
			continue
		}
		labels := d.labelsOf(r.Context(), row.ID)
		if !matchesLabels(labels, want) {
			continue
		}
		out = append(out, d.toAPI(r.Context(), row, owner, d.startOf(r.Context(), row.ID), labels, d.urlAuthOf(r.Context(), row.ID)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d Deps) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), row.ID)
	writeJSON(w, http.StatusOK, d.toAPI(r.Context(), *row, owner, d.startOf(r.Context(), row.ID), d.labelsOf(r.Context(), row.ID), d.urlAuthOf(r.Context(), row.ID)))
}

func (d Deps) handleDestroyMachine(w http.ResponseWriter, r *http.Request) {
	// Resolved before the destroy, not after: a foreign id must never reach
	// the manager, or a tenant could delete another tenant's machine and be
	// told 404 about a machine that is already gone.
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Destroy(r.Context(), r.PathValue("id")); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.Cmd == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "cmd is required", "pass cmd", nil)
		return
	}
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	resp, err := d.Machines.Exec(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleExecStream(w http.ResponseWriter, r *http.Request) {
	row, ok := d.ownedMachine(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	d.execStream(w, r, row.ID)
}

// handleSpriteExec is the sprites-compatible alias.
//
// {name} is a machine NAME, which is what a sprites consumer persists as the
// sprite id; an id-shaped value is tried as an id first. Unknown or foreign is
// a 404 like any other id, never a 403: a 403 would say the name exists.
func (d Deps) handleSpriteExec(w http.ResponseWriter, r *http.Request) {
	id, ok := d.machineIDByName(r.Context(), r.PathValue("name"))
	if !ok {
		notFound(w, "machine")
		return
	}
	row, ok := d.ownedMachine(w, r, id)
	if !ok {
		return
	}
	d.execStream(w, r, row.ID)
}

// execStream is the half both stream routes share. Ownership is settled by the
// caller, so a foreign machine is a 404 whether or not the query is well
// formed.
func (d Deps) execStream(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	if len(q["cmd"]) == 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "cmd is required", "pass cmd", nil)
		return
	}
	// Refused here rather than in the guest, because reaching the guest means
	// waking the machine first: a client that asked for a terminal it cannot
	// type into would pay a wake to be told so.
	if q.Get("tty") == "true" && q.Get("stdin") == "false" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"tty=true cannot be combined with stdin=false",
			"drop stdin=false; a terminal always reads stdin, and both SDKs set it for you", nil)
		return
	}
	if err := d.Machines.ExecStream(w, r, id); err != nil {
		writeMapped(w, err)
	}
}

// machineIDShape matches an id minted by newID("m").
var machineIDShape = regexp.MustCompile(`^m-[0-9a-f]{24}$`)

// machineIDByName resolves the alias's path segment.
//
// The same order the router applies, and for the same reason: an id-shaped
// value that names a row wins, because that is one row read rather than a list
// scan; then the subscription cache, a mutex and a map lookup rather than a
// full-table query over HTTP; then a store scan for the row the subscription
// has not delivered yet. Without the cache step the alias cost a scan on the
// forwarding host AND another on the owner, which made it materially more
// expensive than the /v1/machines/{id}/exec/stream it aliases.
//
// The answer in every case is the live machine of that name with the lowest
// id. Two live rows with one name is a bug elsewhere, and the lowest id is the
// stable answer to it rather than whichever the store listed first.
func (d Deps) machineIDByName(ctx context.Context, name string) (string, bool) {
	if machineIDShape.MatchString(name) {
		if _, err := d.Store.GetMachine(ctx, name); err == nil {
			return name, true
		}
	}
	if d.Lookup != nil {
		if m, ok := d.Lookup(name); ok {
			return m.ID, true
		}
	}
	rows, err := d.Store.ListMachines(ctx)
	if err != nil {
		return "", false
	}
	id := ""
	for _, row := range rows {
		if row.Name != name || row.State == state.StateDestroyed {
			continue
		}
		if id == "" || row.ID < id {
			id = row.ID
		}
	}
	return id, id != ""
}

func (d Deps) handleSuspend(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Suspend(r.Context(), r.PathValue("id")); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleWake(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	if err := d.Machines.Wake(r.Context(), r.PathValue("id")); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRedeploy boots a machine again from another image, in place.
//
// Internal by convention rather than by routing: the rollout is its only
// caller, here or on the host that holds the machine. A peer's call
// authenticates as admin, so the tenancy checks below pass for it exactly as
// they do for the owner of the build.
func (d Deps) handleRedeploy(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	var req RedeployRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.Image == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "image is required",
			"pass image: a rootfs build id from pilot deploy or POST /v1/builds", nil)
		return
	}
	// The build becomes this machine's root filesystem, which is the same
	// tenancy crossing a create would be.
	if !d.ownedBuild(w, r, req.Image) {
		return
	}
	row, err := d.Machines.Redeploy(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d.toAPI(r.Context(), *row, OrgID(r.Context()), d.startOf(r.Context(), row.ID), d.labelsOf(r.Context(), row.ID), d.urlAuthOf(r.Context(), row.ID)))
}

func (d Deps) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req CheckpointRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	// A soft cap: what this checkpoint will weigh is unknown until it is
	// taken, so the check admits while the org is under the line and overshoots
	// by at most one checkpoint. Unbounded checkpointing is the case it
	// catches; the retention policy is what bounds growth in the ordinary one.
	if err := quota.Check(r.Context(), d.Store, actingOrg(r), quota.Delta{SnapshotGiB: 1}); err != nil {
		if writeQuotaError(w, err) {
			return
		}
		writeMapped(w, err)
		return
	}
	ckpt, err := d.Machines.Checkpoint(r.Context(), r.PathValue("id"), req.Comment)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPICheckpoint(*ckpt))
}

func (d Deps) handleListCheckpoints(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	cks, err := d.Machines.ListCheckpoints(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	out := make([]Checkpoint, 0, len(cks))
	for _, c := range cks {
		out = append(out, toAPICheckpoint(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRestoreCheckpoint restores in place: same machine, same URL, same
// token.
//
// A checkpoint id is resolved to its machine BEFORE anything acts on it, and
// the machine is what tenancy is checked against: checkpoints carry no org of
// their own, so a foreign checkpoint id is a foreign machine.
func (d Deps) handleRestoreCheckpoint(w http.ResponseWriter, r *http.Request) {
	ck, err := d.Machines.GetCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	if _, ok := d.ownedMachine(w, r, ck.MachineID); !ok {
		return
	}
	row, err := d.Machines.RestoreCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	owner, _ := d.tenancy().OrgOf(r.Context(), row.ID)
	writeJSON(w, http.StatusOK, d.toAPI(r.Context(), *row, owner, d.startOf(r.Context(), row.ID), d.labelsOf(r.Context(), row.ID), d.urlAuthOf(r.Context(), row.ID)))
}

// handleCheckpointStatus lets a caller learn when a checkpoint became durable.
//
// Checkpoint returns as soon as the guest is running again, with the upload
// still in flight, so this is the only way to know the data can be restored
// from another host.
func (d Deps) handleCheckpointStatus(w http.ResponseWriter, r *http.Request) {
	ck, err := d.Machines.GetCheckpoint(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	if _, ok := d.ownedMachine(w, r, ck.MachineID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, toAPICheckpoint(*ck))
}

// logFollowInterval is how often a follow looks for new console output.
//
// A poll rather than inotify: one bounded read of one local file per open
// tail, and the consumer is a human reading a pane or an agent tailing a boot.
// Nothing reads faster than this, and nothing here is a dependency.
const logFollowInterval = 500 * time.Millisecond

// logRowInterval is how often a follow re-checks that the machine still
// exists.
//
// Deliberately much slower than the file poll, and that gap is the point. The
// file read is local; the row read is a store query, which on a Corrosion host
// is an HTTP round trip to the local agent. Asking both every tick made every
// open tail cost 2 queries per second for its whole life, on every host,
// whether or not the log had grown -- the same cost the router's cache exists
// to keep off the hot path. The row only has to be read often enough to notice
// a destroy, and a few extra seconds of silence before a tail ends is not
// something a reader can tell from the network.
const logRowInterval = 5 * time.Second

// logFollowRetries is how many CONSECUTIVE read failures a follow absorbs
// before it ends.
//
// A transient failure clears on the next tick, and ending a tail there would
// cut a session the caller cannot restart from where it stopped. A persistent
// one never clears: EIO on the state dir, a permission change, a disk that
// went away. Retrying that forever wrote a warn line twice a second for as
// long as the client held the connection, and left the reader watching what
// looked like a machine that had simply gone quiet. Ten ticks is five seconds,
// which is well past transient and well short of a session.
//
// A destroyed machine is not this: the row check ends that tail, and a missing
// file is nil rather than an error.
const logFollowRetries = 10

func (d Deps) handleLogs(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	id := r.PathValue("id")
	logs, err := d.Machines.Logs(r.Context(), id)
	if err != nil {
		writeMapped(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)

	if !r.URL.Query().Has("follow") { // follow=1 and a bare follow both work
		return
	}
	d.followLogs(w, r, id, int64(len(logs)))
}

// followLogs streams the console log as it grows.
//
// It ends on exactly two things: the client leaving, and the machine being
// destroyed. NOT on suspend -- the idle monitor suspends a quiet sandbox after
// a minute, so a follow that ended there would cut every agent's tail one
// minute into a session. A suspended machine keeps its state dir, so the file
// stays where it is and the follow simply sees no delta until the wake.
func (d Deps) followLogs(w http.ResponseWriter, r *http.Request, id string, offset int64) {
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	flush()

	poll, rowPoll := logFollowInterval, logRowInterval
	if d.LogFollowInterval > 0 {
		poll = d.LogFollowInterval
	}
	if d.LogRowInterval > 0 {
		rowPoll = d.LogRowInterval
	}

	fails := 0
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	rowTicker := time.NewTicker(rowPoll)
	defer rowTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-rowTicker.C:
			if _, err := d.Store.GetMachine(r.Context(), id); errors.Is(err, state.ErrNotFound) {
				return // destroyed: the row is gone, and so is the file
			}
			continue
		case <-ticker.C:
		}

		delta, err := d.Machines.LogTail(id, offset)
		if err != nil {
			fails++
			if fails == 1 {
				// Once per run of failures, not once per tick: the same line
				// twice a second for the life of a connection is a log flood,
				// and it says nothing the first one did not.
				slog.Warn("log follow read failed; will retry", "machine", id, "err", err)
			}
			if fails < logFollowRetries {
				continue
			}
			slog.Error("log follow gave up", "machine", id, "failures", fails, "err", err)
			// Said out loud on the stream. A tail that just stops is
			// indistinguishable from a machine that went quiet, and the
			// reader would wait on output that is never coming.
			_, _ = fmt.Fprintf(w, "\n[pilots] log follow ended: %v\n", err)
			flush()
			return
		}
		fails = 0
		if len(delta) == 0 {
			continue
		}
		if _, err := w.Write(delta); err != nil {
			return
		}
		flush()
		offset += int64(len(delta))
	}
}

// toAPIVolume converts a stored volume row to the wire shape.
//
// Sizes are stored in mebibytes and reported in gibibytes, because a volume is
// created in gibibytes and the two must round-trip: a 10 GiB volume that comes
// back as 10240 of something is a client bug waiting to happen.
func toAPIVolume(v state.Volume, orgID string) Volume {
	return Volume{
		ID: v.ID, Name: v.Name, OrgID: orgID, SizeGiB: v.SizeMiB / 1024,
		MachineID: v.MachineID, HostID: v.HostID, MountPath: v.MountPath,
		CreatedAt: v.CreatedAt,
	}
}

func (d Deps) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	var req CreateVolumeRequest
	if err := decodeBody(r, &req); err != nil && !errors.Is(err, io.EOF) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.SizeGiB <= 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "size_gib is required", "pass size_gib", nil)
		return
	}
	req.OrgID = actingOrg(r)
	if !d.checkQuota(w, r, quota.Delta{VolumeGiB: req.SizeGiB}) {
		return
	}
	v, err := d.Machines.CreateVolume(r.Context(), req)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(*v, req.OrgID))
}

func (d Deps) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Machines.ListVolumes(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	org, narrow := listOrg(r)
	out := make([]Volume, 0, len(rows))
	for _, v := range rows {
		owner, ok := d.visible(r, v.ID, org, narrow)
		if !ok {
			continue
		}
		out = append(out, toAPIVolume(v, owner))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMachineVolume reports the volume drive Firecracker is really running.
func (d Deps) handleMachineVolume(w http.ResponseWriter, r *http.Request) {
	if _, ok := d.ownedMachine(w, r, r.PathValue("id")); !ok {
		return
	}
	v, err := d.Machines.MachineVolume(r.Context(), r.PathValue("id"))
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleWhoami echoes the caller's own principal back.
//
// A key can otherwise learn nothing about itself: GET /v1/api-keys needs an
// org to filter by and admin scope to call, and /v1/health is unauthenticated
// so it says nothing about the caller. Without this route the CLI can only
// report what its credentials file happened to record, which is wrong the
// moment PILOT_API_KEY holds a different key.
func (d Deps) handleWhoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, WhoamiResponse{
		OrgID:  OrgID(r.Context()),
		Scopes: Scopes(r.Context()),
		HostID: d.HostID,
	})
}

func (d Deps) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := d.Store.ListHosts(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	// What each host reports about its own capacity, joined in memory. Two
	// list queries rather than two per row: a fleet read must not cost a query
	// per host, and neither of these is large.
	//
	// Both best effort. A host whose capacity row cannot be read is still
	// listed, with zeroes, because the hosts row is the answer to "who is in
	// the fleet" and a missing side table must not remove anyone from it.
	caps := map[string]state.HostCapacity{}
	if rows, err := d.Store.ListHostCapacity(r.Context()); err == nil {
		for _, c := range rows {
			caps[c.HostID] = c
		}
	}
	cached := map[string]int{}
	if rows, err := d.Store.ListHostBuilds(r.Context()); err == nil {
		for _, b := range rows {
			cached[b.HostID] = len(b.Builds)
		}
	}

	const aliveWindow = 30 * time.Second
	out := make([]Host, 0, len(hosts))
	for _, h := range hosts {
		c := caps[h.ID]
		// The capacity row's figure wins when there is one: it is what every
		// ranker actually reads, and reporting the hosts row's older copy here
		// would show an operator a different number from the one placement
		// used.
		free := h.MemFreeMiB
		if c.UpdatedAt > 0 {
			free = c.MemFreeMiB
		}
		out = append(out, Host{
			ID: h.ID, PublicIP: h.PublicIP, WGAddr: h.WGAddr,
			CPUFree: h.CPUFree, MemFreeMiB: free, LastSeen: h.LastSeen,
			Alive:             time.Since(time.Unix(h.LastSeen, 0)) < aliveWindow,
			CPUVendor:         h.Vendor,
			MemReclaimableMiB: c.MemReclaimableMiB,
			VCPUsRunning:      c.VCPUsRunning,
			Draining:          c.Draining,
			BuildsCached:      cached[h.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}
