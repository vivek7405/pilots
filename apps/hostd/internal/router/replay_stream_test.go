package router

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body with no declared length is not read at the edge.
//
// bufferBody read every body to EOF or to the cap before the machine saw a
// byte. For a chunked request that turned a streaming upload into a staged
// one: the app could not begin work until the client had finished sending, and
// a client that streams slowly held the request at the edge for as long as it
// liked. A path that streamed stopped streaming, on every request, to keep a
// feature available that most requests never use.
//
// The assertion is that NOTHING was consumed: the reader is still at its first
// byte when bufferBody returns.
func TestAChunkedBodyIsNotBufferedAtTheEdge(t *testing.T) {
	var read int
	body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
		read++
		if read > 1 {
			return 0, io.EOF
		}
		copy(p, "hello")
		return 5, nil
	}))
	req := httptest.NewRequest(http.MethodPost, "http://alpha/x", body)
	req.ContentLength = -1 // what net/http sets for a chunked request

	buf, err := bufferBody(req)
	if !errors.Is(err, errStreamingBody) {
		t.Fatalf("bufferBody returned %v, want errStreamingBody", err)
	}
	if buf != nil {
		t.Errorf("buffered %d bytes of a streaming body", len(buf))
	}
	if read != 0 {
		t.Errorf("the body was read %d times at the edge; a chunked upload must "+
			"reach the machine as it arrives", read)
	}
	// And the body is still the caller's, untouched, so the machine gets all
	// of it.
	rest, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "hello" {
		t.Errorf("the machine would receive %q, want the whole body", rest)
	}
}

// A bounded body IS buffered, because it can be and the replay needs it. The
// line is boundedness, not size, so this is asserted beside the case above.
func TestABoundedBodyIsStillBuffered(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://alpha/x", strings.NewReader("hello"))

	buf, err := bufferBody(req)
	if err != nil {
		t.Fatalf("bufferBody: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("buffered %q, want the whole body", buf)
	}
}

// A declared body over the cap is refused without being read, and the refusal
// is DISTINCT from the streaming one: a cap is a limit somebody can raise, and
// an undeclared length is a shape no cap would help with.
func TestTheTwoUnreplayableReasonsAreDistinct(t *testing.T) {
	big := httptest.NewRequest(http.MethodPost, "http://alpha/x", strings.NewReader("x"))
	big.ContentLength = replayMaxBody + 1
	_, err := bufferBody(big)
	if err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if errors.Is(err, errStreamingBody) {
		t.Error("an oversized body was reported as a streaming one")
	}
	if !strings.Contains(err.Error(), "over") {
		t.Errorf("the refusal does not mention the limit: %v", err)
	}
}

// readerFunc adapts a function to io.Reader.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
