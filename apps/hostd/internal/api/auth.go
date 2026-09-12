package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	// bearerKey carries the raw key a request authenticated with, for the
	// one handler that has to present it again: the hosted MCP endpoint
	// calls back through the public API as the caller.
	bearerKey
	// bearerHashKey carries its sha256, which is what the limits row is
	// keyed by. Kept rather than recomputed, so a restriction check is a map
	// lookup instead of a hash on the request path.
	bearerHashKey
)

// BearerToken returns the key the request authenticated with, or "" when it
// came in on an exempt path or as a peer.
func BearerToken(ctx context.Context) string {
	key, _ := ctx.Value(bearerKey).(string)
	return key
}

// BearerHash returns the sha256 of that key, which is what a limits or
// revocation row is keyed by. Empty for a peer or an exempt path.
func BearerHash(ctx context.Context) string {
	hash, _ := ctx.Value(bearerHashKey).(string)
	return hash
}

// principal is the authenticated caller. Both halves travel together because
// every authorisation question needs both: which org's rows may be seen, and
// which routes may be called.
type principal struct {
	OrgID  string
	Scopes []string
	// Self is the machine a BROKER token was minted for, empty for every
	// ordinary key. It is what turns an org-wide key into a machine-wide one:
	// reads stay org-wide, because a machine that can see its siblings can do
	// nothing with that alone, and every WRITE is refused on anything but this
	// machine and the service it belongs to.
	Self        string
	SelfService string
}

// Self is the machine a broker token was minted for, or empty.
//
// Empty is the ordinary case and means "not narrowed". A handler asking this
// question must treat empty as "no restriction", never as "no machine".
func Self(ctx context.Context) (machine, service string) {
	p, _ := ctx.Value(principalKey).(principal)
	return p.Self, p.SelfService
}

// OrgID returns the authenticated caller's org, if any.
func OrgID(ctx context.Context) string {
	p, _ := ctx.Value(principalKey).(principal)
	return p.OrgID
}

// Scopes returns a copy of the authenticated caller's scopes. A copy, because
// the slice lives in the request context and a handler that sorted or appended
// to it in place would be editing the principal every later check reads.
func Scopes(ctx context.Context) []string {
	p, _ := ctx.Value(principalKey).(principal)
	return append([]string(nil), p.Scopes...)
}

// HasScope reports whether the caller's key carries a scope, honouring the
// hierarchy: an admin key has every scope.
func HasScope(ctx context.Context, want string) bool {
	p, _ := ctx.Value(principalKey).(principal)
	return rankOf(p.Scopes) >= scopeRank[want] && scopeRank[want] != 0
}

// WithAdminPrincipal returns a context carrying an admin-scoped caller.
//
// The principal is unexported, so this is how a caller outside this package
// builds the context a handler is handed in production: the mesh path above
// uses it, and so does anything mounted behind this middleware that has to
// construct the same value (internal/detect's handler, in its tests).
func WithAdminPrincipal(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey, principal{Scopes: []string{ScopeAdmin}})
}

// WithTenantPrincipal returns a context carrying an ordinary org-scoped
// caller, the counterpart to WithAdminPrincipal.
//
// Same reason that one is exported: the principal type is unexported, so a
// package mounted behind this middleware -- internal/detect serves POST
// /v1/plan -- has no other way to build the context a handler is really given.
// Without it those tests could only ever exercise the admin path, which is
// exactly the path the repository rule does NOT gate.
func WithTenantPrincipal(ctx context.Context, orgID string, scopes ...string) context.Context {
	if len(scopes) == 0 {
		scopes = []string{ScopeDeploy}
	}
	return context.WithValue(ctx, principalKey, principal{OrgID: orgID, Scopes: scopes})
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
	{"/v1/plan", ScopeMachines},
	{"/v1/hosts", ScopeMachines},
	{"/v1/whoami", ScopeMachines},
	// The lowest scope opens the MCP endpoint; each tool then calls its own
	// route back through this table with the same key, so a machines key
	// reaches list_machines and is refused list_services, exactly as it
	// would be over plain HTTP.
	{"/mcp", ScopeMachines},
	{"/v1/builds", ScopeDeploy},
	// Builders belong to the org whose builds run in them, so seeing and
	// resetting one is a deploy-scoped act rather than an administrative one.
	// Without this row the table falls through to admin, and unsticking your
	// own build would need a key that can mint keys.
	{"/v1/builders", ScopeDeploy},
	// A deploy-scoped key READS its own connections here, because a caller
	// refused a {repo, ref} build has to be able to see what it is connected
	// to. Writing one is admin-scoped, checked in handleConnectRepo rather
	// than in this table, which cannot express "this method, not that one".
	{"/v1/repos", ScopeDeploy},
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
	// RFC 9728: an MCP client reads this after a 401 to learn where to log
	// in, so by definition it has no credential yet.
	"/.well-known/oauth-protected-resource":     true,
	"/.well-known/oauth-protected-resource/mcp": true,
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
			unauthorized(w, d.challenge(r))
			return
		}

		// A peer of this fleet, calling over the mesh. Constant-time, marker
		// required, admin because it acts for whichever tenant's rollout or
		// scale decision it carries. No row is read: the secret this is
		// derived from is the one that already unlocks every guest agent on
		// the fleet.
		if d.PeerToken != "" && r.Header.Get(forwardedHeader) != "" &&
			subtle.ConstantTimeCompare([]byte(key), []byte(d.PeerToken)) == 1 {
			next.ServeHTTP(w, r.WithContext(WithAdminPrincipal(r.Context())))
			return
		}

		sum := sha256.Sum256([]byte(key))
		hash := hex.EncodeToString(sum[:])

		// A BROKER token, minted for one machine by its own host. Recognised
		// by its prefix so it is never hashed and looked up as a key -- there
		// is no row to find, which is the point of it being a signed claim.
		if strings.HasPrefix(key, BrokerTokenPrefix) {
			d.authenticateBrokerToken(w, r, next, key, hash)
			return
		}

		rec, err := d.Store.GetAPIKeyByHash(r.Context(), hash)
		if err != nil {
			if !errors.Is(err, state.ErrNotFound) {
				WriteError(w, http.StatusInternalServerError, CodeInternal,
					"auth lookup failed", NextInternal, nil)
				return
			}
			unauthorized(w, d.challenge(r))
			return
		}
		// Cheap guard on the Store contract: the row is looked up by hash, so
		// this should never differ.
		if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) != 1 {
			unauthorized(w, d.challenge(r))
			return
		}

		// Revocation is checked AFTER the key resolves and before anything
		// acts on it. The tombstone replicates like every other row, so a key
		// killed on one host stops working on all of them within gossip
		// latency, with no host having to be reachable for the check.
		revoked, err := d.tenancy().Revoked(r.Context(), hash)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, CodeInternal,
				"auth lookup failed", NextInternal, nil)
			return
		}
		if revoked {
			unauthorized(w, d.challenge(r))
			return
		}

		// A key's LIFETIME, checked in the same breath as its revocation and
		// for the same reason: a credential that outlives what its owner
		// agreed to is the failure both are here to prevent. A key with no
		// limits row is unrestricted, which is every operator key.
		limits, err := d.tenancy().Limits(r.Context(), hash)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			WriteError(w, http.StatusInternalServerError, CodeInternal,
				"auth lookup failed", NextInternal, nil)
			return
		}
		if expired(limits, time.Now()) {
			// A 401 rather than a 403: the credential is no longer valid at
			// all, and a client that sees this should get a new one rather
			// than ask for a wider scope.
			w.Header().Set("WWW-Authenticate", d.challenge(r))
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized,
				"this token expired",
				"authorize the application again, or use a token with no expiry", nil)
			return
		}

		if need, ok := scopeAllows(rec.Scopes, r.URL.Path); !ok {
			WriteError(w, http.StatusForbidden, CodeScopeRequired,
				"scope "+need+" required",
				"mint a key with scope "+need+": POST /v1/api-keys with an admin key", nil)
			return
		}

		ctx := context.WithValue(r.Context(), principalKey,
			principal{OrgID: rec.OrgID, Scopes: splitScopes(rec.Scopes)})
		ctx = context.WithValue(ctx, bearerKey, key)
		ctx = context.WithValue(ctx, bearerHashKey, hash)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PeerTokenFor derives this fleet's peer credential from the agent-token
// secret every host already carries.
//
// One function, so no caller can derive it differently. The same construction
// machines.Manager.token uses for a guest credential under a different label,
// so the two token spaces cannot collide. Empty for an empty secret: a box
// with no secret has no peers, and WithAuth never matches an empty token.
func PeerTokenFor(agentTokenSecret string) string {
	if agentTokenSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(agentTokenSecret))
	_, _ = mac.Write([]byte("hostd-peer"))
	return "peer-" + hex.EncodeToString(mac.Sum(nil))
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

func unauthorized(w http.ResponseWriter, challenge string) {
	w.Header().Set("WWW-Authenticate", challenge)
	WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized",
		"pass an API key: pilot login, or set PILOT_API_KEY", nil)
}

// authenticateBrokerToken verifies a machine's own credential.
//
// Three checks beyond the signature, and all three read LOCAL state, so a
// machine's token stops working everywhere within gossip latency without any
// host having to be reachable:
//
//   - the revocation tombstone, the same one an API key is checked against, so
//     `POST /v1/api-keys/{hash}/revoke` kills a broker token too;
//   - the machine row, which must still exist and must not be destroyed, so
//     destroying a machine ends its tokens at once rather than in fifteen
//     minutes;
//   - the tenancy claim, which must still say what the token says, so a token
//     cannot outlive the org it was issued for.
func (d Deps) authenticateBrokerToken(w http.ResponseWriter, r *http.Request,
	next http.Handler, key, hash string) {
	claims, err := VerifyBrokerToken(d.BrokerKey, key)
	if err != nil {
		unauthorized(w, d.challenge(r))
		return
	}

	revoked, err := d.tenancy().Revoked(r.Context(), hash)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal,
			"auth lookup failed", NextInternal, nil)
		return
	}
	if revoked {
		unauthorized(w, d.challenge(r))
		return
	}

	m, err := d.Store.GetMachine(r.Context(), claims.Machine)
	if err != nil || m.State == state.StateDestroyed {
		// A destroyed machine and a machine this host has never heard of get
		// the same answer, which is the right one: neither is a caller.
		unauthorized(w, d.challenge(r))
		return
	}
	owner, known := d.tenancy().OrgOf(r.Context(), claims.Machine)
	if !known || owner != claims.Org {
		unauthorized(w, d.challenge(r))
		return
	}

	if need, ok := scopeAllows(strings.Join(claims.Scopes, ","), r.URL.Path); !ok {
		WriteError(w, http.StatusForbidden, CodeScopeRequired,
			"scope "+need+" required",
			"grant it: PUT /v1/machines/"+claims.Machine+"/secrets with scopes including "+need, nil)
		return
	}

	ctx := context.WithValue(r.Context(), principalKey, principal{
		OrgID: claims.Org, Scopes: claims.Scopes,
		Self: claims.Machine, SelfService: claims.Service,
	})
	ctx = context.WithValue(ctx, bearerKey, key)
	ctx = context.WithValue(ctx, bearerHashKey, hash)
	next.ServeHTTP(w, r.WithContext(ctx))
}
