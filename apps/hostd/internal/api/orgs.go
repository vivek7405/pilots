package api

import (
	"net/http"
	"sort"
)

// Which orgs a key can act as.
//
// # What this is NOT
//
// It is not a list of teams, and it deliberately cannot become one. Who the
// people are, what a team is called and who belongs to it live in the
// dashboard's own database; the fleet knows an org only as a string on a row.
// A route here that claimed to list somebody's teams would be inventing an
// answer out of the only thing it has, which is "orgs that own something".
//
// # What it is
//
// The orgs this KEY can act as, which is a question the fleet can answer from
// its own rows and is exactly what a client needs to offer a switch:
//
//   - a tenant key acts as exactly one org, its own, and says so;
//   - an admin key acts as any org, so it answers with the ones that own
//     something, which is the set worth switching between.
//
// An admin key that owns nothing yet answers with an empty list rather than an
// error: a fresh fleet has no orgs and that is not a failure.

// OrgsResponse is the orgs a key can act as, and which one it is acting as now.
type OrgsResponse struct {
	// Current is the org this request acted as, so a client can show what it is
	// switching FROM without a second call.
	Current string `json:"current"`
	// Orgs are the ones this key may act as, sorted. One entry for a tenant
	// key; every org that owns something for an admin key.
	Orgs []string `json:"orgs"`
	// Admin says whether this key may act as an org not in the list. Without
	// it, a client cannot tell "these are your options" from "these are the
	// ones we happen to know about".
	Admin bool `json:"admin"`
}

func (d Deps) handleListOrgs(w http.ResponseWriter, r *http.Request) {
	current := actingOrg(r)
	out := OrgsResponse{Current: current, Admin: IsAdmin(r.Context()), Orgs: []string{}}

	if !out.Admin {
		// A tenant key has exactly one org by construction, and listing the
		// fleet's other orgs to it would be a tenant oracle.
		if current != "" {
			out.Orgs = []string{current}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	// For an admin key, the orgs that own something. Read from the local
	// replica like everything else, so this answers on any host and needs
	// nobody to be reachable.
	seen := map[string]bool{}
	machines, err := d.Store.ListMachines(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	for _, m := range machines {
		if org, ok := d.tenancy().OrgOf(r.Context(), m.ID); ok && org != "" {
			seen[org] = true
		}
	}
	services, err := d.Store.ListServices(r.Context())
	if err != nil {
		writeMapped(w, err)
		return
	}
	for _, s := range services {
		if org, ok := d.tenancy().OrgOf(r.Context(), s.ID); ok && org != "" {
			seen[org] = true
		}
	}
	// The current one, always, even when it owns nothing yet: a client offering
	// a switch must be able to show what it is switching from.
	if current != "" {
		seen[current] = true
	}

	for org := range seen {
		out.Orgs = append(out.Orgs, org)
	}
	sort.Strings(out.Orgs)
	writeJSON(w, http.StatusOK, out)
}
