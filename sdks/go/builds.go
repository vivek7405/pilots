package pilots

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
)

// Builds turns any Dockerfile into a bootable rootfs.
type Builds struct{ c *Client }

// BuildStream is a build's NDJSON log.
//
// hostd answers 200 before the build starts, because a client watching a
// ten-minute build needs the first step's output in the first second. The
// consequence is that the status code cannot be the verdict: the LAST line is,
// and Result is what reads it.
type BuildStream struct {
	// ID is also in the X-Pilot-Build-Id header, so a client that loses its
	// connection can reattach without parsing the body.
	ID string
	// Lines yields each line as it arrives. Iterate it once.
	Lines iter.Seq2[BuildLogLine, error]

	seen []BuildLogLine
	// drained is set once the response body has been read to its end, after
	// which Lines replays seen rather than touching the closed body.
	drained bool
}

// BuildOptions is what a build may be asked for beyond its context.
//
// Deploy names a service to cut a release for from the image. The HOST does
// that, on the build's verdict, exactly once -- so a caller that walks away
// mid-build still ends with a release, and two callers watching one build
// still produce one rollout. The release's id arrives on the last log line,
// as BuildLogLine.Release.
type BuildOptions struct {
	// App groups the services a plan produces. Repository builds only.
	App string
	// Deploy is the service id to cut a release for from the image.
	Deploy string
}

// Create uploads a build context (a tar) and streams the build.
func (b *Builds) Create(ctx context.Context, contextTar io.Reader, opts BuildOptions) (*BuildStream, error) {
	req, err := b.c.request(ctx, http.MethodPost,
		query("/v1/builds", [2]string{"deploy", opts.Deploy}), contextTar)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := b.c.send(req)
	if err != nil {
		return nil, err
	}
	return newBuildStream(res, res.Header.Get("X-Pilot-Build-Id")), nil
}

// CreateFromRepo builds a REPOSITORY, naming it rather than uploading it. The
// host fetches the ref through the fleet's GitHub App, plans it, and builds
// the one step a plan may produce, which is the path a push already takes.
//
// The stream is the one Create returns, so a caller reads the verdict the same
// way. A plan with more than one step is refused with plan_multi_service and
// the refusal is readable at the build's log, exactly as a push's is.
func (b *Builds) CreateFromRepo(ctx context.Context, ref RepoRef, opts BuildOptions) (*BuildStream, error) {
	path := query("/v1/builds",
		[2]string{"app", opts.App}, [2]string{"deploy", opts.Deploy})
	body, err := json.Marshal(ref)
	if err != nil {
		return nil, fmt.Errorf("pilots: encoding the repository: %w", err)
	}
	req, err := b.c.request(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.c.send(req)
	if err != nil {
		return nil, err
	}
	return newBuildStream(res, res.Header.Get("X-Pilot-Build-Id")), nil
}

// Logs replays a build's log, following it live when asked. The stream is
// identical to the one Create returned, so a reattach is not a second format.
func (b *Builds) Logs(ctx context.Context, id string, follow bool) (*BuildStream, error) {
	path := "/v1/builds/" + url.PathEscape(id) + "/logs"
	if follow {
		path = query(path, [2]string{"follow", "1"})
	}
	req, err := b.c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	res, err := b.c.send(req)
	if err != nil {
		return nil, err
	}
	return newBuildStream(res, id), nil
}

func newBuildStream(res *http.Response, id string) *BuildStream {
	bs := &BuildStream{ID: id}
	bs.Lines = func(yield func(BuildLogLine, error) bool) {
		// A caller that ranged Lines to render progress and then asks Result
		// for the verdict must not reopen a body the first pass closed, so a
		// second range replays what the first one saw. That is what makes
		// "iterate, then Result()" and "Result() alone" both work.
		if bs.drained {
			for _, line := range bs.seen {
				if !yield(line, nil) {
					return
				}
			}
			return
		}
		defer func() { bs.drained = true; res.Body.Close() }()
		scanner := bufio.NewScanner(res.Body)
		// A build log line carries a whole compiler error; the default 64 KiB
		// ceiling would turn one long line into a silent truncation.
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for scanner.Scan() {
			raw := scanner.Bytes()
			if len(raw) == 0 {
				continue
			}
			var line BuildLogLine
			if err := json.Unmarshal(raw, &line); err != nil {
				yield(BuildLogLine{}, fmt.Errorf("pilots: build log line: %w", err))
				return
			}
			bs.seen = append(bs.seen, line)
			if !yield(line, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(BuildLogLine{}, fmt.Errorf("pilots: reading the build log: %w", err))
		}
	}
	return bs
}

// Result drains the stream and returns the rootfs build id.
//
// It returns a *BuildFailed when the last line carries an error, and equally
// when the stream ended with no verdict at all: an interrupted build must not
// read as a successful one.
func (b *BuildStream) Result() (string, error) {
	for _, err := range b.Lines {
		if err != nil {
			return "", err
		}
	}
	if len(b.seen) == 0 {
		return "", &BuildFailed{ID: b.ID, Reason: "the build stream was empty"}
	}
	last := b.seen[len(b.seen)-1]
	if last.Error != "" {
		return "", &BuildFailed{ID: b.ID, Reason: last.Error, Lines: b.seen}
	}
	if last.Result != "" {
		return last.Result, nil
	}
	return "", &BuildFailed{ID: b.ID, Reason: "the build stream ended without a verdict", Lines: b.seen}
}

// Release is the deployment this build was cut into, once the stream has been
// read.
//
// Non-empty only for a build whose request named a service to deploy: the
// host cuts that release itself and puts its id on the last line.
func (b *BuildStream) Release() string {
	if len(b.seen) == 0 {
		return ""
	}
	return b.seen[len(b.seen)-1].Release
}

// textLines yields each non-empty line of a response body as it arrives.
func textLines(res *http.Response) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		defer res.Body.Close()
		scanner := bufio.NewScanner(res.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for scanner.Scan() {
			if !yield(scanner.Text(), nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield("", fmt.Errorf("pilots: reading the log: %w", err))
		}
	}
}
