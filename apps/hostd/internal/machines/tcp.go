package machines

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// TCPStream carries one TCP connection to a port inside the machine over
// a websocket, by reverse-proxying the upgrade to the guest agent's
// /tcp/{port} exactly the way ExecStream proxies /exec/stream: the same
// token, the same subprotocol handling, the same wake-if-suspended. It is
// the transport under `pilot proxy`.
func (m *Manager) TCPStream(w http.ResponseWriter, r *http.Request, machineID string, port int) error {
	if _, ok := m.get(machineID); !ok {
		if err := m.Wake(r.Context(), machineID); err != nil {
			return err
		}
	}
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
	}
	m.Begin(machineID)
	defer m.End(machineID)
	defer m.Touch(context.WithoutCancel(r.Context()), machineID)

	target := &url.URL{Scheme: "http", Host: slot.AgentAddr()}
	offered := api.OfferedSubprotocol(r)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, target.Host
		out.URL.Path, out.URL.RawPath = "/tcp/"+strconv.Itoa(port), ""
		out.URL.RawQuery = ""
		out.Header.Del("Sec-WebSocket-Protocol")
		out.Header.Set("Authorization", "Bearer "+m.token(machineID))
	}
	proxy.ModifyResponse = func(res *http.Response) error {
		if offered != "" && res.StatusCode == http.StatusSwitchingProtocols {
			res.Header.Set("Sec-WebSocket-Protocol", offered)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("tcp stream to guest failed", "machine", machineID, "port", port, "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"machine unreachable"}`))
	}
	proxy.ServeHTTP(w, r)
	return nil
}
