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
			api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest, err.Error(),
				"pass compose: the file's text", nil)
			return
		}
		if req.Compose == "" {
			api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest, "compose is required",
				"pass compose: the file's text", nil)
			return
		}

		plan, planErr, err := Compile(r.Context(), req)
		switch {
		case planErr != nil:
			// The whole list, in one answer: a caller fixes their file once
			// rather than one key per failed deploy. Its own body shape, not
			// an ErrorResponse, so it goes out through WriteJSON.
			api.WriteJSON(w, http.StatusBadRequest, planErr)
		case err != nil:
			// A file that does not parse, an unset variable, a cycle. All the
			// caller's, all fixable, so all 400.
			api.WriteError(w, http.StatusBadRequest, api.CodeComposeInvalid, err.Error(),
				"fix the file named in error", nil)
		default:
			api.WriteJSON(w, http.StatusOK, plan)
		}
	}
}
