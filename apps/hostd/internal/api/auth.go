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

const orgIDKey ctxKey = iota

// OrgID returns the authenticated caller's org, if any.
func OrgID(ctx context.Context) string {
	org, _ := ctx.Value(orgIDKey).(string)
	return org
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

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), orgIDKey, rec.OrgID)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="pilots"`)
	writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
}
