package github

import (
	"context"
	"log/slog"
	"strings"
)

// The dot beside a commit that says whether this push deployed.
//
// A push deploy had no feedback of any kind. The build ran, it succeeded or it
// did not, and the only way to find out was to notice the application had not
// changed and then go looking for a log that nothing linked to. Every other
// deploy service answers that with a commit status, it costs one API call, and
// it puts the link to the log where a person is already looking.
//
// Four states over a push's life: pending when the build starts, failure when
// the build or the rollout fails, success when a release is cut. A preview
// sandbox reports success with its URL, since that is the thing the author
// wants to click.
//
// Every call here is best effort. A status that cannot be posted must never
// fail a deploy that worked: the deploy is the product, the status is the
// notification, and a repository whose installation lacks the `statuses`
// permission would otherwise have every push fail at the last step.

// status posts one commit status, logging rather than returning a failure.
func (d Deps) status(ctx context.Context, token, repo, sha, state, description, target string) {
	if d.App == nil || token == "" || sha == "" || repo == "" {
		return
	}
	if err := d.App.Status(ctx, token, repo, sha, state, description, target); err != nil {
		// Debug rather than warn for the commonest cause, which is an
		// installation that was never granted the permission: it is a
		// configuration choice, not a fault, and a warning per push would
		// train everyone to ignore the log.
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "404") {
			slog.Debug("could not post a commit status; the installation may not "+
				"grant statuses: write", "repo", repo, "err", err)
			return
		}
		slog.Warn("could not post a commit status", "repo", repo, "sha", sha, "err", err)
	}
}

// buildURL is where a status points: the page that streams this build's log.
//
// Empty when no dashboard is configured, in which case the status still posts
// and simply carries no link. A status with no link is worth more than no
// status, because the state alone answers "did my push deploy".
func (d Deps) buildURL(buildID string) string {
	if d.DashboardURL == "" || buildID == "" {
		return ""
	}
	return strings.TrimSuffix(d.DashboardURL, "/") + "/builds/" + buildID
}
