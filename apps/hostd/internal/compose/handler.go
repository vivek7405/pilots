package compose

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// maxBody is the same 1 MiB cap the rest of the API decodes under. The CLI
// refuses a larger file client-side so the caller is told which file it was
// rather than being handed a 413.
const maxBody = 1 << 20

// Handler serves POST /v1/compose/plan.
//
// It lives here rather than in internal/api because this package imports that
// one for the wire types a Step embeds, and the import cannot go both ways.
// api.Deps takes it as an http.HandlerFunc, the same way the GitHub webhook is
// injected.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error()})
			return
		}
		if req.Compose == "" {
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "compose is required"})
			return
		}

		plan, planErr, err := Compile(r.Context(), req)
		switch {
		case planErr != nil:
			// The whole list, in one answer: a caller fixes their file once
			// rather than one key per failed deploy.
			writeJSON(w, http.StatusBadRequest, planErr)
		case err != nil:
			// A file that does not parse, an unset variable, a cycle. All the
			// caller's, all fixable, so all 400.
			writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error()})
		default:
			writeJSON(w, http.StatusOK, plan)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
