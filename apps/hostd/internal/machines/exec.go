package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// Exec runs a command inside a machine.
//
// A machine that is asleep is woken first: an exec is a use of the machine, so
// it should behave the same as a request arriving at its URL. Waking on exec
// rather than erroring is what lets an agent treat a suspended sandbox as
// simply a slow one.
func (m *Manager) Exec(ctx context.Context, machineID string, req api.ExecRequest) (*api.ExecResponse, error) {
	if _, ok := m.get(machineID); !ok {
		if err := m.Wake(ctx, machineID); err != nil {
			return nil, err
		}
	}

	slot, ok := m.SlotFor(machineID)
	if !ok {
		return nil, fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
	}

	// Count it in flight so the idle monitor cannot suspend the machine
	// mid-command, and record the activity so a long silent build does not
	// look idle.
	m.Begin(machineID)
	defer m.End(machineID)
	defer m.Touch(context.WithoutCancel(ctx), machineID)

	return m.execOnSlot(ctx, machineID, slot, req)
}

// ExecStream proxies the agent's websocket exec stream. The same wake as Exec:
// an open stream is a use of the machine, so a suspended sandbox is woken for
// one rather than refusing it.
func (m *Manager) ExecStream(w http.ResponseWriter, r *http.Request, machineID string) error {
	if _, ok := m.get(machineID); !ok {
		if err := m.Wake(r.Context(), machineID); err != nil {
			return err
		}
	}

	slot, ok := m.SlotFor(machineID)
	if !ok {
		return fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
	}

	m.execStreamAt(w, r, machineID, slot.AgentAddr())
	return nil
}

// execStreamAt is the transport half of ExecStream.
//
// The in-flight and activity bookkeeping lives here rather than in the caller
// because ServeHTTP blocks for the life of the upgraded connection, and that
// whole life is what the idle monitor must count: a guest is never suspended
// mid-command, however long the command runs.
func (m *Manager) execStreamAt(w http.ResponseWriter, r *http.Request, machineID, agentAddr string) {
	m.Begin(machineID)
	defer m.End(machineID)
	defer m.Touch(context.WithoutCancel(r.Context()), machineID)

	target := &url.URL{Scheme: "http", Host: agentAddr}
	// Every offer, not only the one that carries a key: a client is free to
	// authenticate with the Authorization header and still offer a
	// subprotocol, and answering that with none fails the connection.
	offered := api.OfferedSubprotocol(r)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(out *http.Request) {
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, target.Host
		// RawQuery crosses verbatim: cmd, path, dir, env, user and stdin are
		// the compatibility surface, and a rewrite here would break clients
		// that hand-build the URL.
		out.URL.Path, out.URL.RawPath = "/exec/stream", ""
		// The API key must never reach a guest, in either carrier.
		out.Header.Del("Sec-WebSocket-Protocol")
		out.Header.Set("Authorization", "Bearer "+m.token(machineID))
	}
	// ModifyResponse runs BEFORE the 101 is hijacked, which is the only moment
	// the chosen subprotocol can be put on the response. A client that offered
	// one and is answered with none fails the connection, so what goes back is
	// the client's own first offer -- the guest negotiated nothing, because the
	// Director deleted the header before the request left this host.
	proxy.ModifyResponse = func(res *http.Response) error {
		if offered != "" && res.StatusCode == http.StatusSwitchingProtocols {
			res.Header.Set("Sec-WebSocket-Protocol", offered)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Error("exec stream to guest failed", "machine", machineID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"machine unreachable"}`))
	}

	proxy.ServeHTTP(w, r)
}

// execOnSlot is the transport half of Exec, without the wake or the in-flight
// bookkeeping. Used directly by paths that already hold the machine's lock and
// must not recurse back into the lifecycle.
func (m *Manager) execOnSlot(ctx context.Context, machineID string, slot *netns.Slot, req api.ExecRequest) (*api.ExecResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("machines: marshal exec: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+m.token(machineID))

	timeout := 60 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS)*time.Millisecond + 10*time.Second
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("machines: exec in %s: %w", machineID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("machines: exec in %s: agent returned %d", machineID, resp.StatusCode)
	}

	var out api.ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("machines: decode exec response: %w", err)
	}
	return &out, nil
}

// Logs returns a machine's serial console output.
//
// This is the guest's own boot and kernel output, which is why the jailer must
// not be given --daemonize: that would send it to /dev/null and leave no way
// to explain why a machine failed to start.
func (m *Manager) Logs(ctx context.Context, machineID string) ([]byte, error) {
	if _, err := m.opts.Store.GetMachine(ctx, machineID); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(m.stateDir(machineID), "lifecycle.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("machines: read logs for %s: %w", machineID, err)
	}
	return raw, nil
}

// LogTail is Logs from a byte offset, for a follow.
//
// No store read: this is what a follow polls twice a second, and on a
// Corrosion host every store read is an HTTP round trip to the local agent.
// Whether the machine still exists is a separate, far rarer question, asked by
// the follow itself -- a missing file alone cannot answer it, being
// indistinguishable from a machine that has not written anything yet.
func (m *Manager) LogTail(machineID string, offset int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(m.stateDir(machineID), "lifecycle.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // not written yet, or removed a beat before the row
		}
		return nil, fmt.Errorf("machines: read logs for %s: %w", machineID, err)
	}
	defer f.Close()

	// Bounded: a guest that prints a gigabyte between two ticks must not be
	// able to make one read allocate a gigabyte. The rest arrives next tick.
	buf, err := io.ReadAll(io.NewSectionReader(f, offset, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("machines: read logs for %s: %w", machineID, err)
	}
	return buf, nil
}
