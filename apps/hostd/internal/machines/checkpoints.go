package machines

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Checkpoint captures a restorable point without stopping the machine.
//
// Checkpoints are sequential per machine and chain through SourceID, so a
// caller can see which point a later one was taken from. A checkpoint and a
// release are the same artifact, which is what later makes promote and
// rollback the same operation.
func (m *Manager) Checkpoint(ctx context.Context, machineID, comment string) (*state.Checkpoint, error) {
	lock := m.lockFor(machineID)
	lock.Lock()
	defer lock.Unlock()

	fcm, ok := m.get(machineID)
	if !ok {
		return nil, fmt.Errorf("machines: %s is not running: %w", machineID, ErrNotFound)
	}

	existing, err := m.opts.Store.ListCheckpoints(ctx, machineID)
	if err != nil {
		return nil, err
	}
	seq := len(existing) + 1

	var sourceID string
	if len(existing) > 0 {
		sourceID = existing[len(existing)-1].ID
	}

	ckpt := &state.Checkpoint{
		ID:        newID("ck"),
		MachineID: machineID,
		Seq:       seq,
		Comment:   comment,
		SourceID:  sourceID,
		CreatedAt: time.Now().Unix(),
	}

	at := checkpointArtifacts(machineID, ckpt.ID)
	localDir := filepath.Join(m.cacheDir(machineID), "checkpoints", ckpt.ID)

	// Returns as soon as the guest is running again; the upload continues in
	// the background and durability is reported separately.
	if err := fcm.Checkpoint(ctx, m.opts.Uploader, at, localDir); err != nil {
		return nil, err
	}

	if err := m.opts.Store.PutCheckpoint(ctx, ckpt); err != nil {
		return nil, err
	}
	return ckpt, nil
}

// CheckpointStatus reports whether a checkpoint's data is durable yet.
func (m *Manager) CheckpointStatus(machineID, checkpointID string) fc.CheckpointStatus {
	return fc.StatusOf(filepath.Join(m.cacheDir(machineID), "checkpoints", checkpointID))
}

// RestoreCheckpoint rolls a machine back, in place.
//
// In place is the whole point: the SAME machine row, URL and token survive, so
// a client holding the URL sees its machine travel back in time rather than
// being handed a different machine. Restoring an older checkpoint discards
// everything written after it.
func (m *Manager) RestoreCheckpoint(ctx context.Context, checkpointID string) (*state.Machine, error) {
	ckpt, err := m.findCheckpoint(ctx, checkpointID)
	if err != nil {
		return nil, err
	}

	lock := m.lockFor(ckpt.MachineID)
	lock.Lock()
	defer lock.Unlock()

	row, err := m.opts.Store.GetMachine(ctx, ckpt.MachineID)
	if err != nil {
		return nil, err
	}

	// Tear the current instance down first. Its disk and memory are being
	// replaced wholesale.
	if fcm, ok := m.get(row.ID); ok {
		slotIdx := 0
		if fcm.Slot != nil {
			slotIdx = fcm.Slot.Idx
		}
		if err := fcm.Kill(); err != nil {
			return nil, err
		}
		m.drop(row.ID)
		if slotIdx > 0 {
			m.pool.Return(slotIdx)
		}
	}

	fcm, err := m.restore(ctx, row, checkpointArtifacts(row.ID, ckpt.ID))
	if err != nil {
		row.State = StateError
		row.UpdatedAt = time.Now().Unix()
		_ = m.opts.Store.PutMachine(ctx, row)
		return row, err
	}
	m.put(row.ID, fcm)

	row.State = StateRunning
	row.LastActivity = time.Now().Unix()
	row.UpdatedAt = time.Now().Unix()
	if err := m.opts.Store.PutMachine(ctx, row); err != nil {
		return row, err
	}
	return row, nil
}

// ListCheckpoints returns a machine's checkpoints in order.
func (m *Manager) ListCheckpoints(ctx context.Context, machineID string) ([]state.Checkpoint, error) {
	return m.opts.Store.ListCheckpoints(ctx, machineID)
}

// findCheckpoint locates a checkpoint by id across this host's machines.
func (m *Manager) findCheckpoint(ctx context.Context, checkpointID string) (*state.Checkpoint, error) {
	rows, err := m.opts.Store.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		cks, err := m.opts.Store.ListCheckpoints(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cks {
			if c.ID == checkpointID {
				return &c, nil
			}
		}
	}
	return nil, fmt.Errorf("machines: checkpoint %s: %w", checkpointID, ErrNotFound)
}
