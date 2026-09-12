package machines

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/fc"
)

// A journal of the lifecycle operations that are in flight.
//
// # What was already handled, and what was not
//
// Machine ADOPTION works: hostd re-reads its breadcrumbs on start, finds the
// Firecrackers that outlived it, and takes them back. What has no equivalent
// is an operation that was HALF DONE when the daemon stopped. A checkpoint
// whose upload was interrupted leaves a local staging directory and a row that
// says nothing; a restore killed between the old machine's death and the new
// row's write leaves a machine that is neither running nor recorded as
// stopped. Both are silent: nothing errors, and the only symptom is a machine
// that does not come back.
//
// # The shape, and why it cannot grow
//
// One file per machine, beside that machine's other breadcrumbs, holding the
// operation currently in flight: written before the first side effect, removed
// when the operation ends. At most one exists per machine, because lockFor
// serialises operations on a machine, so there is nothing to compact and no
// way for this to grow.
//
// That bound is the point. Fly's version of this feature became fly's version
// of an outage: a Bolt event store grew unbounded on one host until machine
// creates timed out past the API's deadline (infra log, 2026-03-26). A journal
// that is bounded by construction cannot repeat it, and a journal bounded by a
// compaction routine is one bug away from repeating it exactly.
//
// Not a Corrosion table. There is nothing to gossip: the operation belongs to
// the host running it, the artefacts it resumes from are already in object
// storage, and a row would replicate a fact no other host can act on.

// opKind names what was in flight.
type opKind string

const (
	opCheckpoint opKind = "checkpoint"
	opRestore    opKind = "restore"
)

// opFile is the journal's name inside a machine's state directory.
const opFile = "op.json"

// opRecord is one in-flight operation.
type opRecord struct {
	Kind opKind `json:"kind"`
	// ID is the checkpoint the operation is about, which is what a resume
	// needs to find its artefacts.
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	StartedAt int64  `json:"started_at"`
}

// beginOp records an operation before its first side effect.
//
// A failure to write is logged and swallowed rather than failing the
// operation: the caller is a checkpoint or a restore that can perfectly well
// proceed, and refusing it because the journal is unwritable would turn a
// recovery aid into an outage.
func (m *Manager) beginOp(machineID string, kind opKind, id string) {
	dir := m.stateDir(machineID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("could not record an in-flight operation", "machine", machineID, "err", err)
		return
	}
	raw, err := json.Marshal(opRecord{
		Kind: kind, ID: id, MachineID: machineID, StartedAt: time.Now().Unix(),
	})
	if err != nil {
		return
	}
	// Written to a temporary name and renamed, so a host that dies mid-write
	// leaves either the old record or the new one, never half of one. A
	// half-parsed record would be abandoned on the next start, which is the
	// worst of the three outcomes.
	tmp := filepath.Join(dir, "."+opFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		slog.Warn("could not record an in-flight operation", "machine", machineID, "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, opFile)); err != nil {
		slog.Warn("could not record an in-flight operation", "machine", machineID, "err", err)
	}
}

// endOp clears the record. Called on success AND on failure: a failed
// operation has already reported itself to its caller, and leaving the record
// behind would make the next start resume something that was decided.
func (m *Manager) endOp(machineID string) {
	err := os.Remove(filepath.Join(m.stateDir(machineID), opFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not clear an in-flight operation record",
			"machine", machineID, "err", err)
	}
}

// ReadOp returns the operation that was in flight for a machine, if any.
//
// An unreadable record answers nothing and is removed: a record that cannot be
// parsed cannot be resumed, and leaving it would make every subsequent start
// try again.
func (m *Manager) ReadOp(machineID string) (opRecord, bool) {
	path := filepath.Join(m.stateDir(machineID), opFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return opRecord{}, false
	}
	var rec opRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Kind == "" {
		slog.Warn("an in-flight operation record is unreadable; discarding it",
			"machine", machineID)
		_ = os.Remove(path)
		return opRecord{}, false
	}
	rec.MachineID = machineID
	return rec, true
}

// ResumeInterrupted settles every operation that was in flight when this host
// last stopped.
//
// Called once, after adoption, so a machine that is simply still running is
// already registered and is not mistaken for an interrupted operation.
//
// What "settle" means per kind:
//
//   - A checkpoint whose upload finished while hostd was down is recorded as
//     durable, which is the update awaitDurable would have made. One that
//     failed, or whose staging directory is gone, is left alone: the
//     checkpoint row exists and says it is not durable, which is true.
//   - A restore is not re-run. It killed the old instance and may or may not
//     have brought the new one up, and re-running it would restore a machine
//     that is already running. What it needs is for the ROW to stop lying:
//     adoption has just decided whether the machine is alive, so a machine
//     with no live process has its row settled by the normal exit path and
//     this only clears the record.
//
// Either way the record goes, so a host that restarts twice does not carry an
// operation forward forever.
func (m *Manager) ResumeInterrupted(ctx context.Context) int {
	settled := 0
	for _, rec := range m.InterruptedOps() {
		switch rec.Kind {
		case opCheckpoint:
			st := fc.StatusOf(m.checkpointDir(rec.MachineID, rec.ID))
			switch {
			case st.Durable:
				if ck, err := m.findCheckpoint(ctx, rec.ID); err == nil && !ck.Durable {
					ck.Durable = true
					if err := m.opts.Store.PutCheckpoint(ctx, ck); err != nil {
						slog.Error("an interrupted checkpoint finished but its row "+
							"was not updated", "checkpoint", rec.ID, "err", err)
						continue
					}
					slog.Info("an interrupted checkpoint finished while this host "+
						"was down; recorded as durable", "checkpoint", rec.ID,
						"machine", rec.MachineID)
				}
			case st.Failed:
				slog.Warn("a checkpoint interrupted by a host restart had already "+
					"failed its upload", "checkpoint", rec.ID, "machine", rec.MachineID)
			default:
				// Neither: the upload never finished and its staging directory
				// is whatever the kill left. The row says not durable, which is
				// the truth, and a restore will fall back to what is in the
				// bucket.
				slog.Warn("a checkpoint was interrupted by a host restart and did "+
					"not complete; it is not durable", "checkpoint", rec.ID,
					"machine", rec.MachineID)
			}
		case opRestore:
			slog.Warn("a restore was interrupted by a host restart; the machine's "+
				"state is whatever adoption just found", "checkpoint", rec.ID,
				"machine", rec.MachineID)
		}
		m.endOp(rec.MachineID)
		settled++
	}
	return settled
}

// InterruptedOps lists the operations that were in flight when this host last
// stopped, one per machine, in machine-id order.
//
// Read from the state root rather than from the store, because the state root
// is what survived: a machine whose row was never written is precisely the
// case a restore interrupted before its PutMachine, and asking the store for
// it would find nothing.
func (m *Manager) InterruptedOps() []opRecord {
	entries, err := os.ReadDir(m.opts.StateRoot)
	if err != nil {
		return nil
	}
	var out []opRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if rec, ok := m.ReadOp(e.Name()); ok {
			out = append(out, rec)
		}
	}
	return out
}
