package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

type ctxKey int

const principalKey ctxKey = iota

// principal is the authenticated caller. Both halves travel together because
// every authorisation question needs both: which org's rows may be seen, and
// which routes may be called.
type principal struct {
	OrgID  string
	Scopes []string
}

// OrgID returns the authenticated caller's org, if any.
func OrgID(ctx context.Context) string {
	p, _ := ctx.Value(principalKey).(principal)
	return p.OrgID
}

// HasScope reports whether the caller's key carries a scope, honouring the
// hierarchy: an admin key has every scope.
func HasScope(ctx context.Context, want string) bool {
	p, _ := ctx.Value(principalKey).(principal)
	return rankOf(p.Scopes) >= scopeRank[want] && scopeRank[want] != 0
}

// IsAdmin reports whether the caller may act across orgs. Admin is the ops
// org's key: it sees every row, including rows created before tenancy
// existed, and it is the only scope that may mint or revoke a key.
func IsAdmin(ctx context.Context) bool { return HasScope(ctx, ScopeAdmin) }

// The three scopes, nested. Stored comma-separated on the key row and sent as
// a JSON array, so a client never has to know the storage form.
const (
	ScopeMachines = "machines"
	ScopeDeploy   = "deploy"
	ScopeAdmin    = "admin"
)

// scopeRank turns the nesting into a comparison. An unknown name is rank 0,
// which is what makes an unrecognised scope string fail closed rather than be
// treated as harmless.
var scopeRank = map[string]int{
	ScopeMachines: 1,
	ScopeDeploy:   2,
	ScopeAdmin:    3,
}

// ValidScope reports whether a name is one of the three. Used by the mint
// route, so a typo becomes a 400 rather than a key that can do nothing.
func ValidScope(s string) bool { return scopeRank[s] != 0 }

// scopePrefixes maps a route prefix to the scope it needs. Longest match wins,
// so a prefix under an already-mapped one can require more.
var scopePrefixes = []struct {
	prefix string
	need   string
}{
	{"/v1/machines", ScopeMachines},
	{"/v1/checkpoints", ScopeMachines},
	{"/v1/volumes", ScopeMachines},
	{"/v1/sprites", ScopeMachines},
	{"/v1/compose/plan", ScopeMachines},
	{"/v1/hosts", ScopeMachines},
	{"/v1/builds", ScopeDeploy},
	{"/v1/services", ScopeDeploy},
	{"/v1/domains", ScopeDeploy},
	{"/v1/api-keys", ScopeAdmin},
	{"/v1/quotas", ScopeAdmin},
	{"/v1/usage", ScopeAdmin},
}

// scopeAllows reports whether a key's scopes cover a path, and which scope the
// path needs.
//
// A path nothing claims needs admin. That is the fail-closed half and it is
// deliberate: a route added without a line in the table above is reachable by
// the ops org alone until someone notices, rather than by every key on the
// fleet.
func scopeAllows(scopes, path string) (need string, ok bool) {
	need = ScopeAdmin
	best := 0
	for _, p := range scopePrefixes {
		if path == p.prefix || strings.HasPrefix(path, p.prefix+"/") {
			if len(p.prefix) > best {
				best, need = len(p.prefix), p.need
			}
		}
	}
	return need, rankOf(splitScopes(scopes)) >= scopeRank[need]
}

func splitScopes(scopes string) []string {
	var out []string
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// rankOf is the highest rank the key carries. An unknown name contributes
// nothing, so a key whose scopes are all unrecognised can reach no route.
func rankOf(scopes []string) int {
	best := 0
	for _, s := range scopes {
		if r := scopeRank[s]; r > best {
			best = r
		}
	}
	return best
}

// exemptPaths bypass auth: liveness and metrics must answer even to a caller
// with no credentials, because they are how the fleet and the operator see a
// host at all.
var exemptPaths = map[string]bool{
	"/v1/health": true,
	"/metrics":   true,
	// The GitHub webhook carries its own credential: an HMAC over the raw
	// body, which is the only thing GitHub can present. It is verified in the
	// handler before the payload is parsed, and an unverified delivery is
	// refused there with 401. Requiring an API key as well would mean putting
	// one into GitHub's webhook configuration, which is a fleet-wide
	// credential sitting in a third party's settings page.
	"/v1/github/webhook": true,
}

// WithAuth authenticates bearer API keys against the local state replica.
//
// The lookup is deliberately local: key hashes replicate to every host, so
// authentication never makes a network call and survives the loss of any
// host, including whichever one runs the dashboard. Nothing here may start
// depending on a remote service.
func WithAuth(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		key, ok := bearerToken(r)
		if !ok {
			unauthorized(w)
			return
		}

		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])

		rec, err := d.Store.GetAPIKeyByHash(r.Context(), hash)
		if err != nil {
			if !errors.Is(err, state.ErrNotFound) {
				writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "auth lookup failed"})
				return
			}
			unauthorized(w)
			return
		}
		// Cheap guard on the Store contract: the row is looked up by hash, so
		// this should never differ.
		if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
			unauthorized(w)
			return
		}

		// Revocation is checked AFTER the key resolves and before anything
		// acts on it. The tombstone replicates like every other row, so a key
		// killed on one host stops working on all of them within gossip
		// latency, with no host having to be reachable for the check.
		revoked, err := d.tenancy().Revoked(r.Context(), hash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "auth lookup failed"})
			return
		}
		if revoked {
			unauthorized(w)
			return
		}

		if need, ok := scopeAllows(rec.Scopes, r.URL.Path); !ok {
			writeJSON(w, http.StatusForbidden,
				ErrorResponse{Error: "scope " + need + " required"})
			return
		}

		ctx := context.WithValue(r.Context(), principalKey,
			principal{OrgID: rec.OrgID, Scopes: splitScopes(rec.Scopes)})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// subprotocolBearer is how a WebSocket client carries the key.
//
// A browser cannot set an Authorization header on an upgrade, so the key rides
// the subprotocol instead. hostd echoes the offered value on the 101 -- the
// WHATWG algorithm fails a connection whose client offered subprotocols and
// whose server chose none -- and strips it before the request reaches a guest.
const subprotocolBearer = "authorization.bearer."

func bearerToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, found := strings.Cut(h, " ")
		if !found || !strings.EqualFold(scheme, "bearer") || token == "" {
			return "", false
		}
		return token, true
	}

	// No ?token= form on purpose: a credential in a query string lands in
	// access logs, in proxy logs and in shell history.
	if p, ok := BearerSubprotocol(r); ok {
		return strings.TrimPrefix(p, subprotocolBearer), true
	}
	return "", false
}

// BearerSubprotocol returns the offered Sec-WebSocket-Protocol entry that
// carries a key, verbatim, so a proxy can echo exactly what the client offered.
//
// Verbatim matters: the handshake fails unless the server chooses one of the
// values the client actually offered, so echoing the key alone -- or a
// re-spelled entry -- would break every browser client.
func BearerSubprotocol(r *http.Request) (string, bool) {
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		p = strings.TrimSpace(p)
		if key, ok := strings.CutPrefix(p, subprotocolBearer); ok && key != "" {
			return p, true
		}
	}
	return "", false
}

// OfferedSubprotocol returns the FIRST Sec-WebSocket-Protocol entry the client
// offered, verbatim, or "" when it offered none.
//
// This is what a proxy echoes on the 101, and it has to cover every offer
// rather than only the one that carries a key. The WHATWG algorithm fails a
// connection whose client offered subprotocols and whose server chose none, so
// a client that authenticates with the Authorization header and offers, say,
// `pilots.v1` would otherwise be answered with none and drop the connection --
// authenticated, upgraded, and unusable.
//
// The first entry rather than a preferred one: a client lists its offers in
// its own order of preference, and choosing the head is the answer that needs
// no agreement about which names this server knows.
func OfferedSubprotocol(r *http.Request) string {
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return ""
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="pilots"`)
	writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
}
