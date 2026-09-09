package machines

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// Terminal sessions live in the guest agent (cmd/guest-agent/sessions.go);
// hostd only reaches them. Listing and killing are plain JSON calls with
// the machine token; attaching is a websocket reverse-proxied the way an
// exec stream is, and wakes the machine if it is suspended -- a session
// survives a suspend the way every process in the guest does, because a
// suspend is a memory snapshot.

// SessionsJSON returns the agent's session list verbatim.
func (m *Manager) SessionsJSON(ctx context.Context, machineID string) ([]byte, error) {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		if err := m.Wake(ctx, machineID); err != nil {
			return nil, err
		}
		if slot, ok = m.SlotFor(machineID); !ok {
			return nil, fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+slot.AgentAddr()+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token(machineID))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("machines: sessions of %s: %w", machineID, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("machines: sessions of %s: agent returned %d", machineID, res.StatusCode)
	}
	return body, nil
}

// KillSession ends a session's process.
func (m *Manager) KillSession(ctx context.Context, machineID, session string) error {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		return fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+slot.AgentAddr()+"/sessions/"+url.PathEscape(session)+"/kill", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token(machineID))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("machines: kill session %s: %w", session, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("machines: session %s: %w", session, ErrNotFound)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("machines: kill session %s: agent returned %d", session, res.StatusCode)
	}
	return nil
}

// AttachStream reattaches a client to a session, as a websocket carrying
// the same frames an exec stream does.
func (m *Manager) AttachStream(w http.ResponseWriter, r *http.Request, machineID, session string) error {
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
		out.URL.Path, out.URL.RawPath = "/attach", ""
		out.URL.RawQuery = "session=" + url.QueryEscape(session)
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
		slog.Error("attach to guest failed", "machine", machineID, "session", session, "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"machine unreachable"}`))
	}
	proxy.ServeHTTP(w, r)
	return nil
}
