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
}

// Create uploads a build context (a tar) and streams the build.
func (b *Builds) Create(ctx context.Context, contextTar io.Reader) (*BuildStream, error) {
	req, err := b.c.request(ctx, http.MethodPost, "/v1/builds", contextTar)
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
func (b *Builds) CreateFromRepo(ctx context.Context, ref RepoRef, app string) (*BuildStream, error) {
	path := "/v1/builds"
	if app != "" {
		path = query(path, [2]string{"app", app})
	}
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
		defer res.Body.Close()
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
