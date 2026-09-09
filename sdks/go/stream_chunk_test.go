package pilots

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A 70 KiB file pushed over stdin as one Write reached the guest agent as
// one 70 KiB websocket message, over the library's default 32 KiB read
// limit: the agent's read failed, its frame loop ended, and the process saw
// EOF mid-stream. Stdin is chunked so no frame can exceed the limit, and the
// bytes still arrive in order and in full.
func TestStdinWritesAreChunkedBelowTheReadLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 100<<10/16) // 100 KiB
	type got struct {
		frames [][]byte
		body   []byte
	}
	done := make(chan got, 1)
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, _ *http.Request) {
		ctx := context.Background()
		conn.SetReadLimit(32 << 10) // the agent's effective limit
		var g got
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Errorf("read: %v (a frame over the limit?)", err)
				break
			}
			g.frames = append(g.frames, data)
			if len(data) == 1 && data[0] == FrameStdinEOF {
				break
			}
			if data[0] == FrameStdin {
				g.body = append(g.body, data[1:]...)
			}
		}
		done <- g
		_ = conn.Write(ctx, websocket.MessageBinary, frame(FrameExit, "\x00"))
		time.Sleep(50 * time.Millisecond)
	})

	s, err := c.Machines.ExecStream(context.Background(), "m-1", []string{"cat"}, ExecStreamOptions{Stdin: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	n, err := s.Stdin.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if err := s.Stdin.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	g := <-done
	if !bytes.Equal(g.body, payload) {
		t.Fatalf("reassembled %d bytes, want %d, and they differ", len(g.body), len(payload))
	}
	for i, f := range g.frames {
		if len(f) > stdinChunk+1 {
			t.Errorf("frame %d is %d bytes, over the %d chunk", i, len(f), stdinChunk)
		}
	}
	if len(g.frames) < len(payload)/stdinChunk {
		t.Errorf("only %d frames for %d bytes; chunking did not happen", len(g.frames), len(payload))
	}
	if last := g.frames[len(g.frames)-1]; len(last) != 1 || last[0] != FrameStdinEOF {
		t.Errorf("last frame = %v, want the stdin EOF frame", last)
	}
	if _, err := s.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}
