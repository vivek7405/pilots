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
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.OrgID == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "org_id is required", "pass org_id and scopes", nil)
		return
	}
	if len(req.Scopes) == 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "scopes is required", "pass org_id and scopes", nil)
		return
	}
	for _, s := range req.Scopes {
		if !ValidScope(s) {
			// Refused rather than stored: a key carrying a scope nothing
			// recognises reaches no route at all, and the caller would find
			// out only when every call came back 403.
			WriteError(w, http.StatusBadRequest, CodeBadRequest,
				"unknown scope "+s+"; valid scopes are machines, deploy, admin",
				"valid scopes are machines, deploy, admin", nil)
			return
		}
	}

	// The restrictions, validated before anything is minted: a key whose
	// limits were refused after it existed would be a key that authenticates
	// with no limits at all.
	if req.MaxMachines < 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "max_machines cannot be negative",
			"pass a positive max_machines, or leave it out for no cap", nil)
		return
	}
	if req.ExpiresAt != 0 && req.ExpiresAt <= time.Now().Unix() {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "expires_at is already in the past",
			"pass a future unix time, or leave it out for a key that lives until it is revoked", nil)
		return
	}
	if len(req.NamePrefix) > 40 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "name_prefix is too long",
			"use a short prefix, at most 40 characters", nil)
		return
	}

	key, hash, err := MintKey(d.keySource())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not mint a key: "+err.Error(), NextInternal, nil)
		return
	}

	// A revoked hash can never be re-minted. The odds of colliding are nil;
	// the check is here because a tombstone must be the last word on a hash,
	// and a mint that reused one would produce a key that authenticates
	// nowhere while looking perfectly valid.
	if revoked, err := d.tenancy().Revoked(r.Context(), hash); err != nil {
		writeMapped(w, err)
		return
	} else if revoked {
		WriteError(w, http.StatusConflict, CodeConflict, "that key hash is revoked",
			"mint a new key; a revoked hash is never reused", nil)
		return
	}

	now := time.Now().Unix()

	// The LIMITS row is written BEFORE the key row, and the order is the
	// whole safety property: a key that exists without its limits is an
	// unrestricted key, while limits that exist without a key restrict
	// nothing and are collected by the same hash if the mint is retried.
	// Fail in the middle and the caller has no usable key, which is the safe
	// half of the two.
	limits := &state.APIKeyLimits{
		Hash: hash, NamePrefix: req.NamePrefix, MaxMachines: req.MaxMachines,
		ExpiresAt: req.ExpiresAt, CreatedAt: now,
	}
	if limits.Restricted() {
		if err := d.Store.PutAPIKeyLimits(r.Context(), limits); err != nil {
			writeMapped(w, err)
			return
		}
	}

	rec := &state.APIKey{
		Hash: hash, OrgID: req.OrgID,
		Scopes:    strings.Join(req.Scopes, ","),
		CreatedAt: now,
	}
	if err := d.Store.PutAPIKey(r.Context(), rec); err != nil {
		writeMapped(w, err)
		return
	}

	// The plaintext appears here and nowhere else, ever.
	writeJSON(w, http.StatusCreated, APIKeyResponse{
		Key: key, Hash: rec.Hash, OrgID: rec.OrgID,
		Scopes: req.Scopes, CreatedAt: rec.CreatedAt,
		NamePrefix: req.NamePrefix, MaxMachines: req.MaxMachines, ExpiresAt: req.ExpiresAt,
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
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "hash is required", "pass hash", nil)
		return
	}
	now := time.Now().Unix()
	if err := d.Store.PutRevocation(r.Context(), &state.Revocation{Hash: hash, RevokedAt: now}); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RevokeResponse{Hash: hash, RevokedAt: now})
}

func (d Deps) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	org := r.URL.Query().Get("org")
	if org == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "org is required", "pass org", nil)
		return
	}
	rows, err := d.Store.ListAPIKeys(r.Context(), org)
	if err != nil {
		writeMapped(w, err)
		return
	}
	out := make([]APIKeyResponse, 0, len(rows))
	for _, k := range rows {
		item := APIKeyResponse{
			Hash: k.Hash, OrgID: k.OrgID,
			Scopes: splitScopes(k.Scopes), CreatedAt: k.CreatedAt,
		}
		// A restricted key says so in the listing. An operator asking what can
		// reach this org needs to see that a token is limited to an agent's
		// own machines, not merely that it exists.
		if l, err := d.Store.GetAPIKeyLimits(r.Context(), k.Hash); err == nil {
			item.NamePrefix, item.MaxMachines, item.ExpiresAt = l.NamePrefix, l.MaxMachines, l.ExpiresAt
		} else if !errors.Is(err, state.ErrNotFound) {
			writeMapped(w, err)
			return
		}
		// Revoked keys stay in the list. An operator asking "what can reach
		// this org" needs to see that a key was killed, not to find it gone.
		rv, err := d.Store.GetRevocation(r.Context(), k.Hash)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			writeMapped(w, err)
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
