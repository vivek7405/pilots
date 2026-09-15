package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// The host's half of the guest's process surface.
//
// Every call here is a proxy to the agent on the machine's own slot, the same
// shape SessionsJSON takes: wake if it is not running, dial the constant
// address, carry the machine's agent token. The host holds no process state of
// its own, which is deliberate -- the guest is the only thing that knows
// whether a pid is alive, and a second copy of that answer on the host would
// be wrong within a second of being written.

// agentJSON performs one request against a machine's agent and returns the
// body. Shared by the process calls so the wake-then-dial dance exists once.
func (m *Manager) agentJSON(ctx context.Context, machineID, method, path string, body []byte) ([]byte, int, error) {
	slot, ok := m.SlotFor(machineID)
	if !ok {
		if err := m.Wake(ctx, machineID); err != nil {
			return nil, 0, err
		}
		if slot, ok = m.SlotFor(machineID); !ok {
			return nil, 0, fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
		}
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+slot.AgentAddr()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token(machineID))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("machines: %s %s on %s: %w", method, path, machineID, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return out, res.StatusCode, nil
}

// Processes lists what a machine is running.
func (m *Manager) Processes(ctx context.Context, machineID string) ([]byte, error) {
	body, code, err := m.agentJSON(ctx, machineID, http.MethodGet, "/processes", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("machines: processes of %s: agent returned %d", machineID, code)
	}
	return body, nil
}

// ProcessAction starts, stops or restarts one named process.
//
// The action is checked HERE as well as in the guest, because this string
// reaches the agent as a path segment: an unchecked one would let a caller
// address any route the agent serves by naming it as an action.
func (m *Manager) ProcessAction(ctx context.Context, machineID, name, action string) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("machines: unknown process action %q: %w", action, ErrInvalid)
	}
	if name == "" {
		return fmt.Errorf("machines: a process action needs a name: %w", ErrInvalid)
	}
	path := "/processes/" + url.PathEscape(name) + "/" + action
	body, code, err := m.agentJSON(ctx, machineID, http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return fmt.Errorf("machines: %s has no process %q: %w", machineID, name, ErrNotFound)
	}
	if code != http.StatusOK {
		return fmt.Errorf("machines: %s of %s on %s: %s", action, name, machineID, agentError(body, code))
	}
	return nil
}

// ProcessLogs returns one process's captured output.
func (m *Manager) ProcessLogs(ctx context.Context, machineID, name string, tail int) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("machines: process logs need a name: %w", ErrInvalid)
	}
	path := "/processes/" + url.PathEscape(name) + "/logs"
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	body, code, err := m.agentJSON(ctx, machineID, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, fmt.Errorf("machines: %s has no process %q: %w", machineID, name, ErrNotFound)
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("machines: logs of %s on %s: agent returned %d", name, machineID, code)
	}
	return body, nil
}

// agentError pulls the agent's own message out of a JSON error body, so the
// caller sees "process web is already running" rather than "agent returned
// 409". Falls back to the status code when the body is not what was expected.
func agentError(body []byte, code int) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return "agent returned " + strconv.Itoa(code)
}
