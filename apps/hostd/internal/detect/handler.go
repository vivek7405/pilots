package detect

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/build"
	"github.com/vivek7405/pilots/hostd/internal/compose"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Stager stages a repository the fleet's GitHub App can read, returning the
// directory it unpacked to. The CALLER removes it.
//
// Declared here rather than imported, because internal/github imports this
// package and the import cannot go both ways. Nil on a fleet with no App, in
// which case a JSON body answers not_configured rather than the route
// vanishing.
type Stager interface {
	Stage(ctx context.Context, repo, ref string) (string, error)
}

// Handler serves POST /v1/plan.
//
// It lives here rather than in internal/api for the same reason the compose
// handler does: this package imports that one for the wire types, and the
// import cannot go both ways. api.Deps takes it as an http.HandlerFunc.
//
// The body is a tar of the directory, the same shape and the same 2 GiB
// ceiling POST /v1/builds takes, so a caller that can build can plan with the
// bytes it already has.
//
// root is where that tar is unpacked, and it is a parameter rather than the
// process temp dir on purpose. os.MkdirTemp("") resolves to /tmp, which on a
// systemd host is very commonly tmpfs: an authenticated caller with the
// machines scope could then extract 2 GiB into the RAM of a host that is also
// running other tenants' microVMs. The builder stages under the cache root for
// exactly this reason, and the plan route stages beside it. Empty falls back
// to the process temp dir, which is what a test wants.
//
// st answers whether the caller's org may have this fleet fetch a repository
// it named. The STORE and not a copy of the rule: api.AllowRepo is the one
// place that question is answered, and a second implementation here would be
// a second thing to keep in step with the build route.
func Handler(root string, repos Stager, st state.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A repository named rather than sent. The host fetches the bytes
		// through the fleet's GitHub App, the path a push takes, so a caller
		// holding only an App JWT can plan.
		if isJSON(r) {
			dir, ok := stageRepo(w, r, repos, st)
			if !ok {
				return
			}
			defer os.RemoveAll(dir)
			plan(w, r, dir)
			return
		}

		if root != "" {
			if err := os.MkdirAll(root, 0o755); err != nil {
				api.WriteError(w, http.StatusInternalServerError, api.CodeInternal,
					"cannot stage the context: "+err.Error(), api.NextInternal, nil)
				return
			}
		}
		dir, err := os.MkdirTemp(root, "pilot-plan-*")
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
		plan(w, r, dir)
	}
}

// isJSON reports whether the body names a repository rather than carrying a
// tar. The media type, not a sniff of the bytes: a tar whose first bytes
// happen to look like JSON must not change which branch runs.
func isJSON(r *http.Request) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && ct == "application/json"
}

// stageRepo decodes a RepoRef and fetches it, writing the refusal itself when
// it cannot. The caller removes the returned directory.
func stageRepo(w http.ResponseWriter, r *http.Request, repos Stager, st state.Store) (string, bool) {
	var ref api.RepoRef
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ref); err != nil {
		api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest,
			"reading the repository: "+err.Error(), api.NextBadBody, nil)
		return "", false
	}
	if ref.Repo == "" || ref.Ref == "" {
		api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest,
			"repo and ref are both required",
			`send {"repo":"owner/name","ref":"main"}`, nil)
		return "", false
	}
	// The API is the trust boundary: this string is interpolated into GitHub
	// API paths under the fleet's own credential. See api.RepoSlug.
	if !api.RepoSlug.MatchString(ref.Repo) {
		api.WriteError(w, http.StatusBadRequest, api.CodeBadRequest,
			"repo must be owner/name",
			`send {"repo":"owner/name","ref":"main"}`, nil)
		return "", false
	}
	// The same question the build route asks, asked by the same function.
	// Planning leaks a private repository's SHAPE rather than its source,
	// which is a smaller hole than building it -- but it is the same hole, so
	// it gets the same answer rather than a weaker one.
	if !api.AllowRepo(w, r, st, ref.Repo) {
		return "", false
	}
	if repos == nil {
		// 503 and not 501: the route exists and works on a fleet whose hosts
		// carry an App. Naming the tar is what makes this actionable without
		// an operator, since every client that can plan can also send one.
		api.WriteError(w, http.StatusServiceUnavailable, api.CodeNotConfigured,
			"this fleet has no GitHub App, so it cannot fetch a repository",
			"send a tar of the directory, or set PILOT_GITHUB_APP_ID and PILOT_GITHUB_APP_KEY on every host", nil)
		return "", false
	}
	dir, err := repos.Stage(r.Context(), ref.Repo, ref.Ref)
	if err != nil {
		// 502 and not 500: the failure is GitHub's answer, not this host's
		// state, and the caller's next step is to check the App's access to
		// that repository rather than to retry.
		api.WriteError(w, http.StatusBadGateway, api.CodeUnavailable,
			"fetching "+ref.Repo+"@"+ref.Ref+": "+err.Error(),
			"check that the fleet's GitHub App is installed on that repository and the ref exists", nil)
		return "", false
	}
	return dir, true
}

// plan is the answer both bodies share, once the directory exists.
func plan(w http.ResponseWriter, r *http.Request, dir string) {

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
