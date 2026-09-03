package api

import (
	"context"
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// TenancyView answers the two questions every authenticated request asks:
// which org owns this object, and has this key been killed.
//
// An interface because the answer comes from the subscription cache in a
// fleet and from the store on a single box, and the handlers must not be able
// to tell the difference. Both implementations read LOCAL state only: this is
// on the request path, and a request path that can block on another host is
// the control plane this project exists without.
type TenancyView interface {
	OrgOf(ctx context.Context, id string) (string, bool)
	Revoked(ctx context.Context, hash string) (bool, error)
}

// StoreTenancy answers from the state store. Used on a single box, and by the
// tests, where there is no subscription cache to read.
func StoreTenancy(st state.Store) TenancyView { return storeTenancy{st} }

type storeTenancy struct{ st state.Store }

func (t storeTenancy) OrgOf(ctx context.Context, id string) (string, bool) {
	row, err := t.st.GetTenancy(ctx, id)
	if err != nil {
		return "", false
	}
	return row.OrgID, true
}

func (t storeTenancy) Revoked(ctx context.Context, hash string) (bool, error) {
	return t.st.IsRevoked(ctx, hash)
}

// tenancy returns the configured view, or one over the store.
//
// The fallback keeps a Deps built without a Tenancy correct rather than
// panicking: a single-box hostd and every test get the store-backed view,
// which answers the same questions from the same rows.
func (d Deps) tenancy() TenancyView {
	if d.Tenancy != nil {
		return d.Tenancy
	}
	return StoreTenancy(d.Store)
}

// mayAccess reports whether the caller may see an object.
//
// An admin key sees everything, including rows created before tenancy existed
// -- those have no owner, so admin is the only caller that can be shown them
// without handing one tenant's machine to another. Every other caller needs a
// tenancy row naming its own org.
func (d Deps) mayAccess(r *http.Request, id string) bool {
	if IsAdmin(r.Context()) {
		return true
	}
	org, ok := d.tenancy().OrgOf(r.Context(), id)
	return ok && org != "" && org == OrgID(r.Context())
}

// notFound answers for an object the caller may not see.
//
// 404 and never 403, and that is the whole point of this function: a 403 tells
// the caller the id exists, which is a machine-name and service-name oracle
// across tenants. "It is not there" is the only answer that leaks nothing.
func notFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, ErrorResponse{Error: what + " not found"})
}

// ownedMachine resolves a machine the caller is allowed to act on.
func (d Deps) ownedMachine(w http.ResponseWriter, r *http.Request, id string) (*state.Machine, bool) {
	row, err := d.Store.GetMachine(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	if !d.mayAccess(r, id) {
		notFound(w, "machine")
		return nil, false
	}
	return row, true
}

// ownedService resolves a service the caller is allowed to act on.
func (d Deps) ownedService(w http.ResponseWriter, r *http.Request, id string) (*state.Service, bool) {
	svc, err := d.Store.GetService(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return nil, false
	}
	if !d.mayAccess(r, id) {
		notFound(w, "service")
		return nil, false
	}
	return svc, true
}

// ownedVolume resolves a volume the caller is allowed to act on.
func (d Deps) ownedVolume(w http.ResponseWriter, r *http.Request, id string) (*state.Volume, bool) {
	v, err := d.Store.GetVolume(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return nil, false
	}
	if !d.mayAccess(r, id) {
		notFound(w, "volume")
		return nil, false
	}
	return v, true
}

// listOrg is the org a list endpoint should be narrowed to, and whether to
// narrow at all.
//
// An admin key sees every row and may narrow with ?org=. A non-admin's ?org=
// is IGNORED rather than refused: the caller has exactly one org, so the
// parameter can only ever be redundant or wrong, and a 403 there would make a
// client that always sends its own org id fail for no reason.
func listOrg(r *http.Request) (org string, narrow bool) {
	if IsAdmin(r.Context()) {
		if want := r.URL.Query().Get("org"); want != "" {
			return want, true
		}
		return "", false
	}
	return OrgID(r.Context()), true
}

// visible reports whether a listed row belongs in the answer, and returns the
// org that owns it.
//
// The owner is returned rather than looked up a second time by the caller
// rendering the row: a list would otherwise ask the same question twice per
// row, which is what toAPI's own comment says it exists to avoid.
//
// An empty org never matches, for the same reason mayAccess refuses one: a
// row with no owner is admin-only, and a key whose row carries no org must
// not be handed every unowned object on the fleet.
func (d Deps) visible(r *http.Request, id, org string, narrow bool) (owner string, ok bool) {
	owner, found := d.tenancy().OrgOf(r.Context(), id)
	if !narrow {
		return owner, true
	}
	return owner, found && org != "" && owner == org
}
