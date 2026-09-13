package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/cron"
	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Point-in-time copies of a volume.
//
// # Why the owner host serves these
//
// A snapshot is a clone inside the volume's own filesystem, and a volume's
// filesystem is mounted on exactly one host. So unlike almost everything else
// in this API, these are not answerable from a local replica: the host that
// holds the mount is the only one that can take, list or restore a snapshot.
//
// A request that arrives elsewhere is FORWARDED there, the same shape a service
// write takes to its arbiter. Refusing would make the caller responsible for
// discovering which host currently mounts a volume, which is a fact that moves.

// SnapshotResponse is one snapshot of a volume.
type SnapshotResponse struct {
	VolumeID string `json:"volume_id"`
	// Snapshot is the stamp that names it, `20260912T101500Z`. It sorts
	// lexically in time order, so a list needs no separate ordering field.
	Snapshot string `json:"snapshot"`
}

// VolumePolicy is how often a volume is snapshotted and how much is kept.
//
// Two retention numbers rather than one, because they answer different
// questions: how far back at a day's resolution, and how far back at all.
// Keeping the newest KeepDaily snapshots plus the newest of each of the last
// KeepWeekly ISO weeks answers both in space bounded by their sum.
//
// An absent or empty Cron means no schedule, which is what every volume had
// before this existed. Retention of zero and zero keeps EVERYTHING, never
// nothing: an unset policy read as "keep none" would delete a volume's whole
// history the first time the loop ran.
type VolumePolicy struct {
	Cron       string `json:"cron,omitempty"`
	KeepDaily  int    `json:"keep_daily,omitempty"`
	KeepWeekly int    `json:"keep_weekly,omitempty"`
}

// ForkVolumeRequest names the new volume a fork creates. Empty mints one from
// the source's name and the snapshot's stamp.
type ForkVolumeRequest struct {
	Name string `json:"name,omitempty"`
}

// SnapshotListResponse is every snapshot of a volume, newest first.
type SnapshotListResponse struct {
	VolumeID  string   `json:"volume_id"`
	Snapshots []string `json:"snapshots"`
}

func (d Deps) handleCreateVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	stamp, err := d.Machines.SnapshotVolume(r.Context(), v.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, SnapshotResponse{VolumeID: v.ID, Snapshot: stamp})
}

func (d Deps) handleListVolumeSnapshots(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	stamps, err := d.Machines.ListVolumeSnapshots(r.Context(), v.ID)
	if err != nil {
		writeMapped(w, err)
		return
	}
	if stamps == nil {
		stamps = []string{}
	}
	writeJSON(w, http.StatusOK, SnapshotListResponse{VolumeID: v.ID, Snapshots: stamps})
}

func (d Deps) handleRestoreVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	if err := d.Machines.RestoreVolumeSnapshot(r.Context(), v.ID, r.PathValue("stamp")); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, SnapshotResponse{VolumeID: v.ID, Snapshot: r.PathValue("stamp")})
}

// forwardToVolumeOwner sends a volume-scoped operation to the host that mounts
// it.
//
// Returns false when this host is the one, or when the volume is mounted
// nowhere -- in which case this host mounts it for the call, which is what
// Attach on the manager path already does.
func (d Deps) forwardToVolumeOwner(w http.ResponseWriter, r *http.Request, hostID string) bool {
	if hostID == "" || hostID == d.HostID {
		return false
	}
	if r.Header.Get(forwardedHeader) != "" {
		// One hop, for the reason every other forward is one hop: two hosts
		// with briefly disagreeing views would otherwise pass it back and
		// forth until something timed out.
		return false
	}
	if d.Peers == nil {
		return false
	}
	addr, ok := d.Peers.InternalAddr(hostID)
	if !ok {
		return false // no route; let the local attempt fail with a clear error
	}
	target := &url.URL{Scheme: "http", Host: addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
		out.Header.Set(forwardedHeader, d.HostID)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable,
			"the host that mounts this volume is unreachable: "+err.Error(),
			"retry in a minute; a host that stays down has its volumes rescued", nil)
	}
	// No client deadline of our own: a snapshot on a large volume is metadata
	// and fast, but a restore behind a slow mount is not, and an abort halfway
	// through leaves the caller not knowing which image is live.
	proxy.FlushInterval = time.Second
	proxy.ServeHTTP(w, r)
	return true
}

func (d Deps) handleDeleteVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	if err := d.Machines.DeleteVolumeSnapshot(r.Context(), v.ID, r.PathValue("stamp")); err != nil {
		writeMapped(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleForkVolumeSnapshot makes a NEW volume from a snapshot.
//
// Separate from a restore because the two answer different questions. A restore
// puts the data back and throws away what came after; a fork gives you both, so
// the thing you are recovering FROM is still there to compare against.
func (d Deps) handleForkVolumeSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	var req ForkVolumeRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	// A fork is a whole new volume, charged like one: it holds its own copy of
	// the data, so a quota that only counted the original would let one
	// recovery double an org's storage without admitting it.
	if !d.checkQuota(w, r, quota.Delta{VolumeGiB: v.SizeMiB / 1024}) {
		return
	}
	fork, err := d.Machines.ForkVolumeSnapshot(r.Context(), v.ID, r.PathValue("stamp"), req.Name)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPIVolume(*fork, actingOrg(r)))
}

func (d Deps) handleGetVolumePolicy(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// Read from the local replica rather than forwarded: a policy is a row, and
	// every host has it. Only the WRITE has to reach the mounting host, because
	// only that host can act on the schedule.
	p, err := d.Store.GetVolumePolicy(r.Context(), v.ID)
	if err != nil {
		// No policy is not an error: it is what every volume has until somebody
		// sets one, and the honest answer is an empty policy.
		writeJSON(w, http.StatusOK, VolumePolicy{})
		return
	}
	writeJSON(w, http.StatusOK, VolumePolicy{
		Cron: p.Cron, KeepDaily: p.KeepDaily, KeepWeekly: p.KeepWeekly,
	})
}

func (d Deps) handlePutVolumePolicy(w http.ResponseWriter, r *http.Request) {
	v, ok := d.ownedVolume(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if d.forwardToVolumeOwner(w, r, v.HostID) {
		return
	}
	var req VolumePolicy
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, err.Error(), NextBadBody, nil)
		return
	}
	// Validated HERE rather than at the moment it fires. A schedule the parser
	// cannot read would simply never run, and "my backups never happened" is
	// the worst possible way to discover a typo.
	if req.Cron != "" {
		if _, err := cron.Parse(req.Cron); err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest,
				"cron: "+err.Error(),
				"five fields in UTC, or @hourly, @daily, @weekly, @monthly", nil)
			return
		}
	}
	if req.KeepDaily < 0 || req.KeepWeekly < 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"retention cannot be negative",
			"keep_daily and keep_weekly are counts; zero on both keeps everything", nil)
		return
	}

	if err := d.Store.PutVolumePolicy(r.Context(), &state.VolumePolicy{
		VolumeID: v.ID, Cron: req.Cron,
		KeepDaily: req.KeepDaily, KeepWeekly: req.KeepWeekly,
		UpdatedAt: time.Now().Unix(),
	}); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}
