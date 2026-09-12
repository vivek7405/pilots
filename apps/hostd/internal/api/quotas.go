package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/quota"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

func quotaToAPI(q state.Quota) QuotaResponse {
	return QuotaResponse{
		OrgID: q.OrgID, MaxMachines: q.MaxMachines, MaxVCPUs: q.MaxVCPUs,
		MaxMemMiB: q.MaxMemMiB, MaxVolumeGiB: q.MaxVolumeGiB,
		MaxBuilds: q.MaxBuilds, MaxSnapshotGiB: q.MaxSnapshotGiB,
		UpdatedAt: q.UpdatedAt,
	}
}

// handleGetQuota answers an org's limits, or the defaults with updated_at 0.
//
// The defaults are returned rather than a 404, because "this org has no row"
// and "this org has no limits" are not the same thing and only one of them is
// true: an org with no row is held to the defaults.
func (d Deps) handleGetQuota(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if org == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "org is required", "pass org", nil)
		return
	}
	writeJSON(w, http.StatusOK, quotaToAPI(quota.For(r.Context(), d.Store, org)))
}

func (d Deps) handlePutQuota(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	if org == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "org is required", "pass org", nil)
		return
	}
	var req QuotaResponse
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	// Zero is legal and means "hold none of these", which is how an org is
	// frozen. Negative is not: it would read as an unreachable limit and
	// silently admit everything.
	for _, v := range []int{req.MaxMachines, req.MaxVCPUs, req.MaxMemMiB,
		req.MaxVolumeGiB, req.MaxBuilds, req.MaxSnapshotGiB} {
		if v < 0 {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "a quota cannot be negative",
				"every limit is zero or more", nil)
			return
		}
	}

	row := &state.Quota{
		OrgID: org, MaxMachines: req.MaxMachines, MaxVCPUs: req.MaxVCPUs,
		MaxMemMiB: req.MaxMemMiB, MaxVolumeGiB: req.MaxVolumeGiB,
		MaxBuilds: req.MaxBuilds, MaxSnapshotGiB: req.MaxSnapshotGiB,
		UpdatedAt: time.Now().Unix(),
	}
	// Zero means the default here, not zero, unlike every other limit on this
	// body. A client written against the previous shape sends no
	// max_snapshot_gib at all, and reading that as "hold no checkpoints" would
	// freeze an org's checkpoints the first time anybody touched its quota.
	if row.MaxSnapshotGiB == 0 {
		row.MaxSnapshotGiB = quota.Defaults.MaxSnapshotGiB
	}
	if err := d.Store.PutQuota(r.Context(), row); err != nil {
		writeMapped(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quotaToAPI(*row))
}

// writeQuotaError answers a refused create.
//
// 429 rather than 403: the caller is allowed to do this, just not right now or
// not this much. A client that sees 403 stops; one that sees 429 knows to
// destroy something or ask for a bigger limit.
func writeQuotaError(w http.ResponseWriter, err error) bool {
	var ex *quota.Exceeded
	if !errors.As(err, &ex) {
		return false
	}
	// Counted by limit name, not by org: which limit an org keeps hitting is
	// what tells a real capacity problem from a runaway client.
	metrics.QuotaRefusals.With(ex.Quota).Inc()
	writeJSON(w, http.StatusTooManyRequests, QuotaExceededResponse{
		Error: "quota exceeded", Code: CodeQuotaExceeded,
		Next:  "free a " + ex.Quota + ", or raise the org's limit: PUT /v1/quotas/<org> with an admin key",
		Quota: ex.Quota,
		Limit: ex.Limit, Used: ex.Used, Scope: ex.Scope,
	})
	return true
}

// checkQuota runs the org's limits over a request and answers if it refuses.
func (d Deps) checkQuota(w http.ResponseWriter, r *http.Request, delta quota.Delta) bool {
	err := quota.Check(r.Context(), d.Store, actingOrg(r), delta)
	if err == nil {
		return true
	}
	if writeQuotaError(w, err) {
		return false
	}
	writeMapped(w, err)
	return false
}
