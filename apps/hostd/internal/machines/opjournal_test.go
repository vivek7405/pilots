package machines

import (
	"os"
	"path/filepath"
	"testing"
)

func journalManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{opts: Options{
		StateRoot: t.TempDir(),
		CacheRoot: t.TempDir(),
	}}
}

func TestAnOpIsRecordedAndClearedAgain(t *testing.T) {
	m := journalManager(t)

	if _, ok := m.ReadOp("m_1"); ok {
		t.Fatal("a machine with no operation reported one")
	}

	m.beginOp("m_1", opCheckpoint, "ck_7")
	rec, ok := m.ReadOp("m_1")
	if !ok {
		t.Fatal("the operation was not recorded")
	}
	if rec.Kind != opCheckpoint || rec.ID != "ck_7" || rec.MachineID != "m_1" {
		t.Errorf("record = %+v, want a checkpoint of ck_7 on m_1", rec)
	}
	if rec.StartedAt == 0 {
		t.Error("the record has no start time")
	}

	m.endOp("m_1")
	if _, ok := m.ReadOp("m_1"); ok {
		t.Error("the record survived endOp")
	}
	// Clearing twice is not an error: a failed operation and its deferred
	// clear both run.
	m.endOp("m_1")
}

// The bound that matters. Fly's version of this feature became fly's version
// of an outage when its event store grew without limit; one record per machine
// cannot, because operations on a machine are serialised.
func TestOnlyOneOpExistsPerMachine(t *testing.T) {
	m := journalManager(t)

	m.beginOp("m_1", opCheckpoint, "ck_1")
	m.beginOp("m_1", opRestore, "ck_2")

	entries, err := os.ReadDir(m.stateDir("m_1"))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !e.IsDir() {
			files++
		}
	}
	if files != 1 {
		t.Errorf("%d files in the machine's state dir, want exactly one record", files)
	}
	rec, _ := m.ReadOp("m_1")
	if rec.Kind != opRestore || rec.ID != "ck_2" {
		t.Errorf("record = %+v, want the latest operation", rec)
	}
}

// A record that cannot be parsed cannot be resumed, and leaving it would make
// every subsequent start try again.
func TestAnUnreadableRecordIsDiscarded(t *testing.T) {
	m := journalManager(t)
	dir := m.stateDir("m_1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, opFile)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := m.ReadOp("m_1"); ok {
		t.Error("an unreadable record was returned as an operation")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the unreadable record was left on disk to be retried forever")
	}
}

func TestInterruptedOpsListsEveryMachine(t *testing.T) {
	m := journalManager(t)
	m.beginOp("m_1", opCheckpoint, "ck_1")
	m.beginOp("m_2", opRestore, "ck_2")
	// A machine with a state dir and no operation must not appear.
	if err := os.MkdirAll(m.stateDir("m_3"), 0o755); err != nil {
		t.Fatal(err)
	}

	ops := m.InterruptedOps()
	if len(ops) != 2 {
		t.Fatalf("InterruptedOps = %+v, want the two machines with records", ops)
	}
	seen := map[string]opKind{}
	for _, op := range ops {
		seen[op.MachineID] = op.Kind
	}
	if seen["m_1"] != opCheckpoint || seen["m_2"] != opRestore {
		t.Errorf("InterruptedOps = %+v", ops)
	}
}

// Whatever a resume decides, the record goes: a host that restarts twice must
// not carry the same operation forward forever.
func TestResumeClearsEveryRecord(t *testing.T) {
	m := journalManager(t)
	m.beginOp("m_1", opCheckpoint, "ck_1")
	m.beginOp("m_2", opRestore, "ck_2")

	if n := m.ResumeInterrupted(t.Context()); n != 2 {
		t.Errorf("ResumeInterrupted settled %d, want 2", n)
	}
	if ops := m.InterruptedOps(); len(ops) != 0 {
		t.Errorf("records survived the resume: %+v", ops)
	}
	// And a second start finds nothing to do.
	if n := m.ResumeInterrupted(t.Context()); n != 0 {
		t.Errorf("a second resume settled %d operations", n)
	}
}
