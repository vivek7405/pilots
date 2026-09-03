package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// KeyPrefix is what every pilots API key starts with, so a leaked one is
// recognisable in a log or a repository scan.
const KeyPrefix = "pilot_"

// MintKey returns a fresh key and its hash.
//
// 24 bytes of crypto/rand rendered as 48 hex characters. The plaintext is
// returned to exactly one caller, once, and never stored: only the hash is
// written, hashed the same way WithAuth hashes an incoming bearer token.
//
// Shared with `hostd bootstrap-key` on purpose. Two implementations of "how a
// key is made" would be two chances for the hash to be computed differently,
// and the symptom is a key that authenticates nowhere.
func MintKey(r io.Reader) (key, hash string, err error) {
	buf := make([]byte, 24)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", "", err
	}
	key = KeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(key))
	return key, hex.EncodeToString(sum[:]), nil
}

func (d Deps) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req CreateAPIKeyRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "bad request body"})
		return
	}
	if req.OrgID == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "org_id is required"})
		return
	}
	if len(req.Scopes) == 0 {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "scopes is required"})
		return
	}
	for _, s := range req.Scopes {
		if !ValidScope(s) {
			// Refused rather than stored: a key carrying a scope nothing
			// recognises reaches no route at all, and the caller would find
			// out only when every call came back 403.
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Error: "unknown scope " + s + "; valid scopes are machines, deploy, admin"})
			return
		}
	}

	key, hash, err := MintKey(d.keySource())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			ErrorResponse{Error: "could not mint a key: " + err.Error()})
		return
	}

	// A revoked hash can never be re-minted. The odds of colliding are nil;
	// the check is here because a tombstone must be the last word on a hash,
	// and a mint that reused one would produce a key that authenticates
	// nowhere while looking perfectly valid.
	if revoked, err := d.tenancy().Revoked(r.Context(), hash); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	} else if revoked {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: "that key hash is revoked"})
		return
	}

	rec := &state.APIKey{
		Hash: hash, OrgID: req.OrgID,
		Scopes:    strings.Join(req.Scopes, ","),
		CreatedAt: time.Now().Unix(),
	}
	if err := d.Store.PutAPIKey(r.Context(), rec); err != nil {
		writeStoreError(w, err)
		return
	}

	// The plaintext appears here and nowhere else, ever.
	writeJSON(w, http.StatusCreated, APIKeyResponse{
		Key: key, Hash: rec.Hash, OrgID: rec.OrgID,
		Scopes: req.Scopes, CreatedAt: rec.CreatedAt,
	})
}

// handleRevokeAPIKey kills a key by writing a tombstone.
//
// Idempotent, and an unknown hash still answers 200: revocation is a row that
// appears, so "revoke this" always has the same meaning and the same effect,
// and refusing an unknown hash would turn the endpoint into an oracle for
// which hashes exist.
func (d Deps) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "hash is required"})
		return
	}
	now := time.Now().Unix()
	if err := d.Store.PutRevocation(r.Context(), &state.Revocation{Hash: hash, RevokedAt: now}); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RevokeResponse{Hash: hash, RevokedAt: now})
}

func (d Deps) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	org := r.URL.Query().Get("org")
	if org == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "org is required"})
		return
	}
	rows, err := d.Store.ListAPIKeys(r.Context(), org)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]APIKeyResponse, 0, len(rows))
	for _, k := range rows {
		item := APIKeyResponse{
			Hash: k.Hash, OrgID: k.OrgID,
			Scopes: splitScopes(k.Scopes), CreatedAt: k.CreatedAt,
		}
		// Revoked keys stay in the list. An operator asking "what can reach
		// this org" needs to see that a key was killed, not to find it gone.
		rv, err := d.Store.GetRevocation(r.Context(), k.Hash)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			writeStoreError(w, err)
			return
		}
		if rv != nil {
			item.RevokedAt = rv.RevokedAt
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// keySource is crypto/rand unless a test replaced it.
func (d Deps) keySource() io.Reader {
	if d.KeySource != nil {
		return d.KeySource
	}
	return rand.Reader
}
