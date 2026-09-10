package machines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

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
		// rows and cols travel with it: the agent resizes the session's
		// window to the terminal that is arriving, and dropping them here
		// left a reattached full-screen program drawn to the old size.
		q := url.Values{"session": {session}}
		if in := r.URL.Query(); in.Get("rows") != "" && in.Get("cols") != "" {
			q.Set("rows", in.Get("rows"))
			q.Set("cols", in.Get("cols"))
		}
		out.URL.RawQuery = q.Encode()
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

// sessionProbeTimeout bounds the one call the idle monitor makes into a guest.
// A guest that cannot answer in this long is not going to, and the monitor
// must not stall its whole tick on it.
const sessionProbeTimeout = 2 * time.Second

// sessionsBusy asks the guest whether any of its terminal sessions still has
// a command running, which the guest reads from its process tree
// (cmd/guest-agent/busy.go).
//
// This is the signal hostd cannot see on its own: it counts a session as
// activity only while a client is attached, because the websocket is what it
// has. An agent that starts a build in a console and detaches has a machine
// that is busy by every reasonable measure and idle by every one hostd can
// take, and suspending it mid-build is indefensible.
//
// It FAILS OPEN: any error -- the guest unreachable, a bad status, a body that
// does not parse -- is "nothing is busy". That is the opposite of guestLoad
// in cmd/hostd (blind means held), and both are right. A replica suspended
// under a live database session loses somebody's transaction, which cannot be
// taken back; a sandbox suspended when a probe failed is a memory snapshot
// that resumes exactly where it was the moment anything touches it. The
// reversible mistake is the one to make.
//
// The agent address is a parameter, the way execStreamAt's is, so the probe
// can be pointed at a fake guest in a test.
func (m *Manager) sessionsBusy(ctx context.Context, machineID, agentAddr string) bool {
	ctx, cancel := context.WithTimeout(ctx, sessionProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+agentAddr+"/sessions", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+m.token(machineID))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var list []struct {
		Busy  bool `json:"busy"`
		Ended bool `json:"ended"`
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return false
	}
	for _, s := range list {
		if s.Busy && !s.Ended {
			return true
		}
	}
	return false
}
