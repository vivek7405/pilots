package api

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
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
