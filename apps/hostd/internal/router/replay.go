package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Letting an application hand a request to a different machine.
//
// # The problem it solves
//
// An app often knows something the router cannot: this request belongs to
// tenant X, whose data lives on machine X; this write must go to the primary;
// this session is pinned to the machine that started it. Without a way to say
// so, every such app builds its own proxy layer inside the guest and pays a
// second hop for every request, or the platform grows a routing rule per use
// case.
//
// Fly answered this with fly-replay and it is the single most-copied idea in
// their routing layer, because it inverts the question: the app does not have
// to know how to reach the other machine, only which one it wants.
//
// # The shape
//
// An app answers with a header naming where the request should have gone, and
// the EDGE replays it there. Once. The response the app sent is discarded
// before any of it reaches the client, so a replay is invisible: the client
// sees one request and one response.
//
// Two forms, both deliberately narrow:
//
//	Pilot-Replay: machine=<name>     send it to this machine
//	Pilot-Replay: elsewhere=true     send it to any other replica of this service
//
// plus an optional `state=<opaque>` the app can read back on the second pass,
// which is how it says "I already looked this up, do not look again".
//
// # What it will not do
//
// A machine may only name a machine in its OWN org, and in its own app or
// service. That is the whole security model and it is checked at the edge, not
// trusted from the header: a header is written by a customer's process, so
// treating it as authority over routing would let one tenant's app aim traffic
// at another tenant's machine.
//
// It replays ONCE. A second header on the second answer is stripped and
// ignored, because two applications each pointing at the other is a loop the
// platform would otherwise run until something times out.
//
// The body is buffered up to a megabyte so it can be sent twice. A larger
// request is refused rather than silently replayed with an empty body, which
// would be a data-loss bug that looks like an application bug.

const (
	// ReplayHeader is what an application answers with to redirect a request.
	ReplayHeader = "Pilot-Replay"
	// ReplaySrcHeader tells the second machine where the request came from,
	// and carries back whatever state the first one set.
	ReplaySrcHeader = "Pilot-Replay-Src"
	// replayMaxBody is the largest request that can be replayed. Past it the
	// body cannot be buffered, and replaying without it would change the
	// request.
	replayMaxBody = 1 << 20
	// replayMaxState bounds the opaque value an app may carry across the
	// replay. It rides in a header, so it is small by construction.
	replayMaxState = 256
)

// errReplay is the sentinel a proxy's response hook returns to abort writing a
// response that is about to be replayed. Nothing has reached the client at
// that point, so the ErrorHandler stays silent.
var errReplay = errors.New("router: replaying")

// replayState rides one request's context.
type replayState struct {
	// want is the header the answering machine set, empty when it set none.
	want string
	// done is the loop guard: true once a replay has happened, which makes
	// every later header on this request inert.
	done bool
	// answeredBy is the machine that set the header, for the org and app check.
	answeredBy *Target
}

type replayCtxKey struct{}

// withReplayState attaches a fresh state to a request's context.
func withReplayState(ctx context.Context, st *replayState) context.Context {
	return context.WithValue(ctx, replayCtxKey{}, st)
}

// replayStateOf returns the state riding this request, or nil.
func replayStateOf(ctx context.Context) *replayState {
	st, _ := ctx.Value(replayCtxKey{}).(*replayState)
	return st
}

// captureReplay is the ModifyResponse every proxy installs.
//
// It always strips the header, so a client never sees an internal routing
// instruction. When the request is replayable and the header is present, it
// records it and returns errReplay, which stops the response from reaching the
// writer.
func captureReplay(ctx context.Context, target *Target) func(*http.Response) error {
	return func(resp *http.Response) error {
		header := resp.Header.Get(ReplayHeader)
		resp.Header.Del(ReplayHeader)
		if header == "" {
			return nil
		}
		st := replayStateOf(ctx)
		if st == nil || st.done {
			// No state means this is an internal hop: the header rides back to
			// the edge, which is the one that replays. Done means one replay
			// has already happened and this is the loop guard.
			if st == nil {
				resp.Header.Set(ReplayHeader, header)
			}
			return nil
		}
		st.want, st.answeredBy = header, target
		// Nothing of this response may reach the client.
		resp.Body.Close()
		return errReplay
	}
}

// replayRequest is a parsed Pilot-Replay header.
type replayRequest struct {
	// Machine names one machine by NAME. Empty when Elsewhere is set.
	Machine string
	// Elsewhere asks for any other replica of the same service.
	Elsewhere bool
	// State is opaque to the platform and echoed to the second machine.
	State string
}

// parseReplay reads the header, or says why it will not.
//
// Deliberately strict. This is a routing instruction from a customer's
// process, so a spelling the platform does not understand is refused rather
// than guessed at: guessing would mean a typo silently routing traffic
// somewhere nobody intended.
func parseReplay(header string) (replayRequest, error) {
	var out replayRequest
	if len(header) > 1024 {
		return out, errors.New("header is too long")
	}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return out, fmt.Errorf("%q is not key=value", part)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "machine":
			out.Machine = value
		case "elsewhere":
			out.Elsewhere = value == "true" || value == "1"
		case "state":
			if len(value) > replayMaxState {
				return out, fmt.Errorf("state is %d bytes, over the %d-byte limit",
					len(value), replayMaxState)
			}
			out.State = value
		default:
			return out, fmt.Errorf("unknown key %q; want machine, elsewhere or state", key)
		}
	}
	if out.Machine == "" && !out.Elsewhere {
		return out, errors.New("names neither a machine nor elsewhere")
	}
	if out.Machine != "" && out.Elsewhere {
		return out, errors.New("names both a machine and elsewhere")
	}
	return out, nil
}

// srcHeader is what the second machine is told about the first.
func srcHeader(from *Target, hostID, state string) string {
	var b strings.Builder
	if from != nil {
		b.WriteString("machine=" + from.Machine.Name)
	}
	b.WriteString(";host=" + hostID)
	b.WriteString(";t=" + strconv.FormatInt(time.Now().UnixMicro(), 10))
	if state != "" {
		b.WriteString(";state=" + state)
	}
	return b.String()
}

// bufferBody reads a request body so it can be sent twice, or reports that it
// is too large to replay.
//
// A request over the cap is NOT silently replayed with an empty body. That
// would be a data-loss bug wearing an application bug's clothes: the second
// machine would receive a POST with no content and answer something plausible.
func bufferBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, replayMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > replayMaxBody {
		return nil, fmt.Errorf("request body is over %d bytes", replayMaxBody)
	}
	return buf, nil
}

// rewind puts a buffered body back on a request so it can be sent again.
func rewind(req *http.Request, body []byte) {
	if body == nil {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}
