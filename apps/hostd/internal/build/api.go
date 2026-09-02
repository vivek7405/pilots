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
