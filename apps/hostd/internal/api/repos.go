package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Naming a repository is an authorization question, and this file is the one
// place it is answered.
//
// The fleet's GitHub App holds an installation token for every repository it
// is installed on. `POST /v1/builds` and `POST /v1/plan` take a {repo, ref}
// body and fetch with that token, so without a rule any key could name any of
// those repositories, build it, and exec into another tenant's source under
// the fleet's own credential. The rule is a row in `repo_links`, read from
// LOCAL state: a host answers "may this org fetch this repository?" from its
// own replica, never from the dashboard's database and never from a peer
// (ARCHITECTURE.md invariant 2).

// AllowRepo reports whether this caller may name a repository, writing the
// refusal itself when it may not.
//
// Exported and store-taking because internal/detect serves POST /v1/plan and
// imports this package -- the import cannot go the other way, and the rule
// must not exist twice. Two copies of "who may fetch a repository" is exactly
// the drift that would leave one route open after the other was closed.
//
// An admin key is allowed, as it is everywhere else on this API: it is the ops
// org's key, it mints and revokes credentials for every org, and it is what
// the dashboard and `pilot deploy` hold today. That is unchanged by this
// function -- what changes is that a TENANT key is no longer refused outright
// (the PR #88 gate) but asked a question it can answer.
func AllowRepo(w http.ResponseWriter, r *http.Request, st state.Store, repo string) bool {
	if IsAdmin(r.Context()) {
		return true
	}
	org := actingOrg(r)
	if org != "" && st != nil {
		_, err := st.GetRepoLink(r.Context(), org, repo)
		if err == nil {
			return true
		}
		if !errors.Is(err, state.ErrNotFound) {
			// A store that cannot answer is not an authorisation to proceed.
			// Fail closed: the alternative is that a wedged replica hands one
			// tenant another tenant's private source.
			WriteError(w, http.StatusInternalServerError, CodeInternal,
				"cannot read this org's connected repositories: "+err.Error(),
				NextInternal, nil)
			return false
		}
	}
	// 403 and not 404: the repository's existence is GitHub's to leak, not
	// ours, and a caller told "not found" would go looking for a typo instead
	// of connecting the repository. The next step is the whole point of the
	// code -- an agent reading this can act on it without a human.
	WriteError(w, http.StatusForbidden, CodeRepoNotConnected,
		"this org has no claim on "+repo+", so this fleet will not fetch it",
		`connect it first: POST /v1/repos {"repo":"`+repo+`"} with an admin-scoped key, `+
			"or send a tar of the directory instead", nil)
	return false
}

// handleConnectRepo records that an org may have this fleet fetch a repository.
//
// ADMIN-SCOPED, deliberately, and the asymmetry with the read is the design:
// the proof that an org controls a repository lives at GitHub -- the App
// installation, bound to an org by the dashboard's install callback -- and
// hostd cannot check it from a request. So the claim is asserted by a party
// that can prove it (the dashboard, an operator, the CLI) and hostd records it
// once; FETCHING then needs no admin key at all, which is what makes a
// tenant-scoped deploy from a repository possible for the first time.
//
// Idempotent: the row is write-once, so connecting twice answers 201 with the
// row that is already there rather than moving it.
func (d Deps) handleConnectRepo(w http.ResponseWriter, r *http.Request) {
	if !IsAdmin(r.Context()) {
		WriteError(w, http.StatusForbidden, CodeScopeRequired,
			"connecting a repository needs an admin-scoped key",
			"ask an operator to connect it, or use a key with scope admin", nil)
		return
	}

	var req ConnectRepoRequest
	if err := decodeBody(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "bad request body", NextBadBody, nil)
		return
	}
	if req.Repo == "" {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "repo is required",
			`send {"repo":"owner/name"}`, nil)
		return
	}
	// The same shape check the build route runs, for the same reason: this
	// string is compared against one that is interpolated into GitHub API
	// paths under the fleet's credential, and a row keyed by a shape the
	// fetch would refuse is a row that can only ever confuse.
	if !RepoSlug.MatchString(req.Repo) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "repo must be owner/name",
			`send {"repo":"owner/name"}`, nil)
		return
	}

	org := actingOrg(r)
	if org == "" {
		// An admin key with no ?org= has no org of its own to connect FOR.
		// Refused rather than written under the ops org, which would be a row
		// no tenant can ever use and nobody can delete.
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"an admin key must say which org it is connecting the repository for",
			"add ?org=<org id> to the request", nil)
		return
	}

	link := &state.RepoLink{
		ID: state.RepoLinkID(org, req.Repo), OrgID: org,
		Repo: state.NormalizeRepo(req.Repo), ConnectedAt: time.Now().Unix(),
	}
	if err := d.Store.PutRepoLink(r.Context(), link); err != nil {
		writeMapped(w, err)
		return
	}
	// Read back rather than echo: the write is ON CONFLICT DO NOTHING, so the
	// row that is there may be older than this request, and the caller should
	// be told when the connection was actually made.
	if got, err := d.Store.GetRepoLink(r.Context(), org, req.Repo); err == nil {
		link = got
	}
	writeJSON(w, http.StatusCreated, RepoLinkResponse{
		Repo: link.Repo, OrgID: link.OrgID, ConnectedAt: link.ConnectedAt,
	})
}

// handleListRepos lists the repositories the caller's org may name.
//
// Readable with a deploy-scoped key, unlike the connect above: a tenant that
// is refused a build needs to be able to see what it IS connected to, and a
// list nobody but an operator can read makes the refusal unactionable.
func (d Deps) handleListRepos(w http.ResponseWriter, r *http.Request) {
	org, narrow := listOrg(r)
	if !narrow {
		org = "" // an admin with no ?org= sees every connection on the fleet
	}
	links, err := d.Store.ListRepoLinks(r.Context(), org)
	if err != nil {
		writeMapped(w, err)
		return
	}
	out := RepoLinkListResponse{Repos: make([]RepoLinkResponse, 0, len(links))}
	for _, l := range links {
		out.Repos = append(out.Repos, RepoLinkResponse{
			Repo: l.Repo, OrgID: l.OrgID, ConnectedAt: l.ConnectedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
