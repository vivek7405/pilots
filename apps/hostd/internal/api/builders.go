package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/quota"
)

// Builders as something an org can see and act on.
//
// A builder is an ordinary machine with an extraordinary job: it is created by
// hostd rather than by a person, it is exempt from the org's quota, the router
// refuses to route to it, and it is per org PER HOST, so an org building on
// three hosts has three. Until now none of that was visible: the only way to
// know a builder existed was to notice a machine in the list nobody made.
//
// Two routes, which is what fly's builder page turns out to need: see them,
// and reset one. Reset is the older of the two requests any build service
// gets, and here it means destroy this org's builder on this host AND advance
// the cache epoch so every OTHER host drops its copy of the layer cache at its
// next build. No message is sent to those hosts; see internal/build/epoch.go
// for why a reset is a write rather than a broadcast.
//
// They are org-scoped, not admin-scoped: a builder belongs to the org whose
// builds it runs, and needing an admin key to unstick your own build is the
// kind of thing that turns a two-minute fix into a support ticket.

// handleListBuilders lists this org's builders across the fleet.
func (d Deps) handleListBuilders(w http.ResponseWriter, r *http.Request) {
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	org, narrow := listOrg(r)
	out := make([]Machine, 0, 4)
	for _, row := range rows {
		if !isBuilder(row.Name) {
			continue
		}
		owner, ok := d.visible(r, row.ID, org, narrow)
		if !ok {
			continue
		}
		out = append(out, d.toAPI(row, owner, d.startOf(r.Context(), row.ID), nil, ""))
	}
	// By host, so two reads of an unchanged fleet agree and the dashboard's
	// table does not reorder itself under the reader.
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostID != out[j].HostID {
			return out[i].HostID < out[j].HostID
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"builders": out})
}

// handleResetBuilder destroys this org's builder on one host and advances the
// org's cache epoch.
//
// The epoch moves even when there is no builder on that host to destroy. The
// two halves answer different complaints -- "this builder is wedged" and "my
// layers are wrong" -- and a caller with the second problem should not have to
// know which host holds a machine in order to fix it.
func (d Deps) handleResetBuilder(w http.ResponseWriter, r *http.Request) {
	if d.Builds == nil {
		WriteError(w, http.StatusServiceUnavailable, CodeNotConfigured,
			"this host has no object storage, so it has no build cache to reset",
			"run this against a host with PILOT_S3_BUCKET set", nil)
		return
	}
	org := actingOrg(r)
	if org == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"a builder reset is scoped to one org and this key names none",
			"use an org-scoped key, or pass ?org=", nil)
		return
	}
	host := r.PathValue("host")

	// Destroy first, then bump. The other order would advance the epoch and
	// then fail, leaving every host in the fleet to re-pull a cache for a
	// builder that is still wedged.
	destroyed := 0
	rows, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	for _, row := range rows {
		if !isBuilder(row.Name) || row.HostID != host {
			continue
		}
		if owner, ok := d.tenancy().OrgOf(r.Context(), row.ID); !ok || owner != org {
			continue
		}
		if err := d.Machines.Destroy(r.Context(), row.ID); err != nil {
			writeMapped(w, err)
			return
		}
		destroyed++
	}

	epoch, err := d.Builds.BumpEpoch(r.Context(), org)
	if err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "epoch": epoch, "destroyed": destroyed,
	})
}

// isBuilder reports whether a machine name is a builder's.
//
// The prefix comes from internal/quota rather than internal/machines, which
// owns the naming: machines imports this package, so the dependency can only
// run one way, and quota is where the constant already lives because the quota
// loop has to exempt these rows. machines/name_test.go asserts the two agree.
func isBuilder(name string) bool {
	return strings.HasPrefix(name, quota.BuilderNamePrefix)
}

// includeBuilders reports whether a machine listing was asked for builders.
//
// Absent by default, because a builder is infrastructure rather than something
// the org made: an agent listing machines to pick one to exec into should not
// have to know to skip it, and a dashboard list that shows it invites someone
// to destroy the thing their next deploy needs.
func includeBuilders(r *http.Request) bool {
	for _, v := range r.URL.Query()["include"] {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == "builders" {
				return true
			}
		}
	}
	return false
}
