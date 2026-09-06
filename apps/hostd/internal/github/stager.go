package github

import (
	"context"
	"io"
	"os"
)

// Stager is what POST /v1/plan and POST /v1/builds reach for when their body
// names a repository instead of carrying a tar.
//
// A type of its own rather than passing Deps around, because those two routes
// need exactly two operations out of the whole delivery path and must not be
// able to reach the rest of it. It is injected as an interface on both routes,
// as Plan, Compose and GitHub already are: internal/api and internal/detect
// cannot import this package, which imports both of them.
type Stager struct{ d Deps }

// NewStager returns nil when no GitHub App is configured, which is what makes
// the routes answer not_configured rather than panicking on a fleet that has
// no App. The caller must keep the nil VISIBLE: assigning a nil *Stager to an
// interface field makes a non-nil interface holding a nil pointer, and the
// 503 branch would then never run.
func NewStager(d Deps) *Stager {
	if d.App == nil {
		return nil
	}
	return &Stager{d: d}
}

// Stage fetches and unpacks a ref, returning the directory. The CALLER removes
// it. Installation 0: a caller naming a repository has no delivery to read an
// installation id from, so it is resolved from the repository.
func (s *Stager) Stage(ctx context.Context, repo, ref string) (string, error) {
	return s.d.Stage(ctx, 0, repo, ref)
}

// Context stages a ref, plans it, and returns the build context tar for the
// one step a plan may produce, recorded under the build id so a refusal reads
// back at GET /v1/builds/{id}/logs exactly as a push's does.
//
// The directory is removed before the tar is returned. detect.TarDir has
// already written the whole archive to a temp file by then, so the reader
// outlives the directory it was made from, and a caller that streams a
// two-minute build is not holding an unpacked copy of the repository for the
// duration.
func (s *Stager) Context(ctx context.Context, id, repo, ref, app string) (io.ReadCloser, error) {
	dir, err := s.d.Stage(ctx, 0, repo, ref)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	tar, _, err := s.d.ContextOf(ctx, id, dir, repo, app)
	if err != nil {
		return nil, err
	}
	return tar, nil
}
