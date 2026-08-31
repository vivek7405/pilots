package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
