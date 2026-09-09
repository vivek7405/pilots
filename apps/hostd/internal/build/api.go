package build

import (
	"context"
	"io"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// The adapter between the builder and hostd's HTTP surface.
//
// It lives here rather than in the api package because the dependency only
// goes one way: build knows the log-line contract, and api must not know what
// a BuildKit is. The interface these satisfy is api.BuildRunner.

// NewBuildID mints an id for a build that is about to start.
func (b *Builder) NewBuildID() string { return NewID() }

// StartBuild runs a build to completion, streaming lines to emit.
func (b *Builder) StartBuild(ctx context.Context, id string, contextTar io.Reader,
	emit func(api.BuildLogLine)) (string, error) {

	res, err := b.Build(ctx, id, contextTar, emit)
	if err != nil {
		return "", err
	}
	return res.RootfsBuildID.String(), nil
}

// RecordRefusal writes a failed build log for a build that never ran.
//
// The push path can refuse before it builds: a plan with more than one
// service, a compose file the planner will not accept, a repository no recipe
// knows. A refusal cannot go through StartBuild, because an empty context
// fails inside the builder with a message about the context rather than about
// the refusal. So it gets its own one-line log under the id the delivery
// minted, and GET /v1/builds/{id}/logs answers for a refused push exactly as
// it answers for a failed build. Otherwise the only record is a journal line
// on whichever host happened to act.
func (b *Builder) RecordRefusal(id string, line api.BuildLogLine) {
	log := b.logs.create(id)
	log.Append(line)
	log.Close()
}

// The builder can carry a build through to the release it was asked for: see
// Log.Hold. Asserted here rather than left to a type assertion at the call
// site, because api.handleBuild takes that interface optionally and a builder
// that quietly stopped satisfying it would deploy with nobody able to watch.
var _ api.BuildLogHolder = (*Builder)(nil)

// HoldLog keeps a build's log open past the build itself, for a caller that
// will append the verdict of what it did with the image. Called BEFORE
// StartBuild, because the build creates the log it writes to.
func (b *Builder) HoldLog(id string) { b.logs.create(id).Hold() }

// RecordLine appends a line to a build's log, where every follower of
// GET /v1/builds/{id}/logs reads it. For the lines that come after the build:
// the builder records its own.
func (b *Builder) RecordLine(id string, line api.BuildLogLine) {
	if log, ok := b.logs.get(id); ok {
		log.Append(line)
	}
}

// ReleaseLog ends a hold and lets the build's followers go.
func (b *Builder) ReleaseLog(id string) {
	if log, ok := b.logs.get(id); ok {
		log.Release()
	}
}

// BuildLog returns a build's recorded output and, when following, a channel of
// what comes after it.
//
// The two are taken together, which is the only reason a late follower cannot
// miss a line. A build this host no longer holds reports not-found rather than
// an empty log: those are different answers, and a client that cannot tell
// them apart concludes a build produced no output.
func (b *Builder) BuildLog(ctx context.Context, id string, follow bool) (
	[]api.BuildLogLine, <-chan api.BuildLogLine, bool) {

	log, ok := b.Log(id)
	if !ok {
		return nil, nil, false
	}
	if !follow {
		lines, _ := log.Snapshot()
		return lines, nil, true
	}
	backlog, live := log.Follow(ctx)
	return backlog, live, true
}
