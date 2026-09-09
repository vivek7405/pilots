package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// handleTCP is GET /tcp/{port}: one TCP connection to a port inside this
// machine, carried as binary websocket frames. It is what `pilot proxy`
// rides -- a local listener opens one of these per accepted connection --
// and what an ssh ProxyCommand rides under -W. Any protocol, not only
// HTTP: the agent copies bytes and understands none of them.
//
// Authenticated by the machine token like every other route, so only
// hostd on the owning host can open one, and hostd only opens one for a
// caller that owns the machine.
func handleTCP(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "port must be 1-65535"})
		return
	}
	// Dial before upgrading, so a port nothing listens on is a plain 502
	// the caller can read rather than a websocket that closes at once.
	dialer := net.Dialer{Timeout: 5 * time.Second}
	tcp, err := dialer.DialContext(r.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "nothing is listening on port " + strconv.Itoa(port)})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		tcp.Close()
		log.Printf("guest-agent: tcp %d: ws accept: %v", port, err)
		return
	}
	defer conn.CloseNow()
	// Bigger than the default 32 KiB: a bulk transfer (a database dump, a
	// file over scp) arrives in whatever chunks the client sends.
	conn.SetReadLimit(4 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ws := websocket.NetConn(ctx, conn, websocket.MessageBinary)

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tcp, ws); _ = tcp.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ws, tcp); done <- struct{}{} }()
	<-done
	tcp.Close()
	cancel()
	<-done
	_ = conn.Close(websocket.StatusNormalClosure, "")
}
