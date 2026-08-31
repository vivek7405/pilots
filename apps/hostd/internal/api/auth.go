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
			// Compare against a dummy anyway so a miss and a hit cost the same.
			subtle.ConstantTimeCompare([]byte(hash), []byte(strings.Repeat("0", len(hash))))
			unauthorized(w)
			return
		}
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
