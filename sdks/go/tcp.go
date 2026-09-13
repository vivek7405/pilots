package pilots

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
)

// TCP opens one TCP connection to a port inside the machine and returns it
// as a net.Conn. The bytes travel as binary websocket frames through hostd
// to the guest agent, which dials 127.0.0.1:port and copies; neither end
// understands the protocol, so a Postgres client, a debugger or ssh work
// the same way. The machine is woken if it is suspended.
//
// Closing the conn closes the tunnel. The ctx bounds the dial; the conn
// outlives it the way ExecStream does, so a caller can dial with a timeout
// and then keep the session.
func (m *Machines) TCP(ctx context.Context, id string, port int) (net.Conn, error) {
	u := strings.Replace(m.c.baseURL, "http", "ws", 1) + "/v1/machines/" + url.PathEscape(id) + "/tcp/" + strconv.Itoa(port)
	if m.c.org != "" {
		u += "?org=" + url.QueryEscape(m.c.org)
	}
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + m.c.credential()}},
	})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	return &chunkedConn{Conn: websocket.NetConn(context.WithoutCancel(ctx), conn, websocket.MessageBinary)}, nil
}

// chunkedConn splits each Write into frames no bigger than stdinChunk, for
// the same reason stdin is chunked: a hop reading with the library's default
// 32 KiB limit drops the whole connection on a larger message, and a bulk
// transfer writes large buffers.
type chunkedConn struct {
	net.Conn
}

func (c *chunkedConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), stdinChunk)
		if _, err := c.Conn.Write(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
