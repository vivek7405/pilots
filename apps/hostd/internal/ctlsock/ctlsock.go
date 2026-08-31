// Package ctlsock is the control channel between hostd and the handler
// processes that serve a machine's disk and memory.
//
// Those handlers are separate processes so that restarting hostd does not take
// every running machine down with it. The cost of that isolation is that state
// hostd needs at snapshot time -- which disk blocks were written, whether every
// memory page is resident -- lives in another address space. This is how it is
// asked for.
//
// The protocol is deliberately tiny: one line in, a status line and an
// optional payload out, one command per connection. It carries a bitmap and an
// acknowledgement, not a service.
package ctlsock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds both ends of a control exchange.
//
// Generous, because one of the commands installs a machine's entire memory
// image before returning.
const Timeout = 60 * time.Second

// Handler answers one command. A non-nil payload is sent after the status
// line; a nil payload is a bare acknowledgement.
type Handler func(cmd string) ([]byte, error)

// Listen creates a control socket.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("ctlsock: mkdir for %s: %w", path, err)
	}
	// A socket left by a handler that was SIGKILLed refuses to bind, which
	// would make a machine that died badly impossible to start again. Removing
	// it is safe: a live handler holds the device, not the file.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("ctlsock: clear %s: %w", path, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("ctlsock: listen on %s: %w", path, err)
	}
	return ln, nil
}

// Serve answers control requests until the listener closes.
func Serve(ln net.Listener, h Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // the listener was closed: the handler is shutting down
		}
		handle(conn, h)
		conn.Close()
	}
}

func handle(conn net.Conn, h Handler) {
	_ = conn.SetDeadline(time.Now().Add(Timeout))

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}

	payload, err := h(strings.TrimSpace(line))
	if err != nil {
		// Newlines would be read as the end of the status line.
		fmt.Fprintf(conn, "ERR %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	fmt.Fprintf(conn, "OK %d\n", len(payload))
	if len(payload) > 0 {
		_, _ = conn.Write(payload)
	}
}

// Request sends one command and returns its payload.
func Request(sock, cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", sock, Timeout)
	if err != nil {
		return nil, fmt.Errorf("ctlsock: dial %s: %w", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(Timeout))

	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return nil, fmt.Errorf("ctlsock: send %s: %w", cmd, err)
	}

	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("ctlsock: read %s status: %w", cmd, err)
	}
	status = strings.TrimSpace(status)

	if reason, ok := strings.CutPrefix(status, "ERR "); ok {
		return nil, fmt.Errorf("ctlsock: %s refused: %s", cmd, reason)
	}
	rest, ok := strings.CutPrefix(status, "OK ")
	if !ok {
		return nil, fmt.Errorf("ctlsock: malformed %s response %q", cmd, status)
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return nil, fmt.Errorf("ctlsock: malformed %s length %q", cmd, rest)
	}
	if n == 0 {
		return nil, nil
	}

	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("ctlsock: read %s payload: %w", cmd, err)
	}
	return payload, nil
}
