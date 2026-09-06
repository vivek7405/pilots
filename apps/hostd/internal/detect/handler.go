package detect

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/compose"
)

// Handler serves POST /v1/plan.
//
// It lives here rather than in internal/api for the same reason the compose
// handler does: this package imports that one for the wire types, and the
// import cannot go both ways. api.Deps takes it as an http.HandlerFunc.
//
// The body is a tar of the directory, the same shape and the same 2 GiB
// ceiling POST /v1/builds takes, so a caller that can build can plan with the
// bytes it already has.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dir, err := os.MkdirTemp("", "pilot-plan-*")
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, api.CodeInternal,
				"cannot stage the context: "+err.Error(), api.NextInternal, nil)
			return
		}
		defer os.RemoveAll(dir)

		body := http.MaxBytesReader(w, r.Body, api.MaxBuildContext)
		if err := build.ExtractContext(body, dir, api.MaxBuildContext); err != nil {
			api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest,
				"reading the context: "+err.Error(),
				"send a tar of the directory; pilot deploy and the build tool make one", nil)
			return
		}

		res, planErr, unknown, err := Plan(r.Context(), dir, Options{
			App: r.URL.Query().Get("app"),
			Env: loadDotEnv(filepath.Join(dir, ".env")),
		})
		switch {
		case planErr != nil:
			// Its own body shape, listing every offending key at once, so a
			// caller fixes the file in one pass rather than one key per try.
			api.WriteJSON(w, http.StatusBadRequest, planErr)
		case unknown != nil:
			api.WriteError(w, http.StatusBadRequest, api.CodeUnknownFramework,
				unknown.Error(),
				"add a Dockerfile, or run pilot mcp and ask your agent to write one from details",
				unknown.Details)
		case err != nil:
			api.WriteError(w, http.StatusBadRequest, api.CodeComposeInvalid, err.Error(),
				"fix the file named in error", nil)
		default:
			api.WriteJSON(w, http.StatusOK, compose.PlanResponse{
				Plan: res.Plan, Detected: res.Detected,
			})
		}
	}
}
