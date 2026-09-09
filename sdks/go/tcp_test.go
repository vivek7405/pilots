package pilots

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The tunnel is a net.Conn: bytes written arrive as binary frames and bytes
// framed back read as a stream, with the key on the dial and the port in
// the path. The server here echoes, which is enough to see both directions
// and the framing boundary disappear.
func TestTCPIsANetConnOverBinaryFrames(t *testing.T) {
	seenPath := make(chan string, 1)
	c := wsServer(t, func(t *testing.T, conn *websocket.Conn, r *http.Request) {
		seenPath <- r.URL.Path
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		ctx := context.Background()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				t.Errorf("frame type %v, want binary", typ)
			}
			if err := conn.Write(ctx, websocket.MessageBinary, data); err != nil {
				return
			}
		}
	})
	conn, err := c.Machines.TCP(context.Background(), "m-1", 5432)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if p := <-seenPath; p != "/v1/machines/m-1/tcp/5432" {
		t.Errorf("path = %q", p)
	}
	payload := bytes.Repeat([]byte("pg"), 50000) // 100 KB, several frames
	go func() { _, _ = conn.Write(payload) }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the bytes that came back differ")
	}
}
