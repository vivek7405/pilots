package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/usage"
)

// UsageSource is this host's meter. An interface so the API can be tested
// without a ledger on disk, and nil on a host that has none.
type UsageSource interface {
	Sum(since, until int64) (map[string]usage.Totals, error)
}

// defaultUsageWindow is what a caller gets for asking nothing. The dashboard's
// poller always sends a range; a person with curl usually does not.
const defaultUsageWindow = 24 * 60 * 60

// handleUsage answers what this host metered over a range.
//
// Every host serves this from its OWN ledger and never asks a peer: the
// dashboard polls each live host once a minute and sums, so no host's answer
// depends on any other host being alive. That is the same rule the rest of the
// API follows, applied to billing.
func (d Deps) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	until, err := unixParam(q.Get("until"), time.Now().Unix())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "until: " + err.Error()})
		return
	}
	since, err := unixParam(q.Get("since"), until-defaultUsageWindow)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "since: " + err.Error()})
		return
	}
	if since >= until {
		writeJSON(w, http.StatusBadRequest,
			ErrorResponse{Error: "since must be before until"})
		return
	}

	// Initialised, not left nil. The poller tolerates a null map; the CSV
	// export downstream of it does not, and an org with no usage is a real
	// answer rather than a missing one.
	out := UsageResponse{
		HostID: d.HostID, Since: since, Until: until,
		Orgs: map[string]UsageTotals{},
	}
	if d.Usage != nil {
		totals, err := d.Usage.Sum(since, until)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			return
		}
		for org, t := range totals {
			out.Orgs[org] = UsageTotals{
				MachineSeconds:   t.MachineSeconds,
				VCPUSeconds:      t.VCPUSeconds,
				MiBSeconds:       t.MiBSeconds,
				VolumeGiBSeconds: t.VolumeGiBSeconds,
			}
		}
	}
	// The range this host actually summed, echoed back: the poller advances
	// its per-host watermark from the answer rather than from what it asked
	// for, so a host that clamped the window must say so.
	writeJSON(w, http.StatusOK, out)
}

// unixParam reads a unix-seconds query parameter, or its default when absent.
func unixParam(raw string, def int64) (int64, error) {
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}
