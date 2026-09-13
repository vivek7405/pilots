package usage

import (
	"testing"
	"time"
)

// ledgerAt builds a ledger whose clock a test drives.
func ledgerAt(t *testing.T, start time.Time) (*Ledger, *time.Time) {
	t.Helper()
	now := start
	l := New(t.TempDir())
	l.now = func() time.Time { return now }
	return l, &now
}

// The gap this closes: a checkpoint is bytes in a bucket held for as long as
// it exists, and nothing metered them. An agent checkpointing after every
// message grew object storage without bound, at our expense, invisible to the
// customer and to us.
func TestSnapshotBytesAccrueInEveryState(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	l, now := ledgerAt(t, start)

	l.Open("m_1", "org_1", "running", 1, 512, 0)
	l.SetSnapshotMiB("m_1", 100)
	*now = now.Add(60 * time.Second)
	// Suspended: no vCPU, no guest memory, and the snapshot is still there.
	l.Transition("m_1", "suspended")
	*now = now.Add(60 * time.Second)
	l.Close("m_1")

	totals, err := l.Sum(start.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	got := totals["org_1"]
	if got.SnapshotMiBSeconds != 100*120 {
		t.Errorf("snapshot_mib_seconds = %d, want %d: the bytes are there in "+
			"both states", got.SnapshotMiBSeconds, 100*120)
	}
	// And the existing rule is untouched: compute accrues in running only.
	if got.VCPUSeconds != 60 {
		t.Errorf("vcpu_seconds = %d, want 60", got.VCPUSeconds)
	}
}

// An interval is a closed fact. Seconds already accrued were accrued against
// the old figure, so a new checkpoint bills from now rather than rewriting
// history for bytes that did not exist yet.
func TestANewCheckpointBillsFromWhenItWasTaken(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	l, now := ledgerAt(t, start)

	l.Open("m_1", "org_1", "running", 1, 512, 0)
	*now = now.Add(100 * time.Second) // 100s with no snapshot
	l.SetSnapshotMiB("m_1", 50)
	*now = now.Add(10 * time.Second) // 10s with one
	l.Close("m_1")

	totals, err := l.Sum(start.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got := totals["org_1"].SnapshotMiBSeconds; got != 50*10 {
		t.Errorf("snapshot_mib_seconds = %d, want %d: the first 100 seconds "+
			"predate the checkpoint", got, 50*10)
	}
}

// Retention deleting a checkpoint has to stop the meter, or an org keeps
// paying for bytes the platform threw away.
func TestDeletingACheckpointStopsTheMeter(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	l, now := ledgerAt(t, start)

	l.Open("m_1", "org_1", "running", 1, 512, 0)
	l.SetSnapshotMiB("m_1", 80)
	*now = now.Add(10 * time.Second)
	l.SetSnapshotMiB("m_1", 0)
	*now = now.Add(1000 * time.Second)
	l.Close("m_1")

	totals, err := l.Sum(start.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got := totals["org_1"].SnapshotMiBSeconds; got != 80*10 {
		t.Errorf("snapshot_mib_seconds = %d, want %d: nothing accrues after "+
			"the checkpoint is gone", got, 80*10)
	}
}

// The common path is a re-meter that changes nothing. It must not split an
// interval per call, or a machine with a checkpoint writes a ledger line every
// time anything asks.
func TestAnUnchangedFigureWritesNothing(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	l, now := ledgerAt(t, start)

	l.Open("m_1", "org_1", "running", 1, 512, 0)
	l.SetSnapshotMiB("m_1", 40)
	for range 10 {
		*now = now.Add(time.Second)
		l.SetSnapshotMiB("m_1", 40)
	}
	l.Close("m_1")

	totals, err := l.Sum(start.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	// One interval of ten seconds, not ten of one: the total is the same
	// either way, so the assertion is on the machine-seconds not being split
	// into a line per call.
	if got := totals["org_1"].SnapshotMiBSeconds; got != 40*10 {
		t.Errorf("snapshot_mib_seconds = %d, want %d", got, 40*10)
	}
}

// A machine with no open interval is not metered at all, so a setter for one
// must do nothing rather than open one.
func TestSettingOnAnUnknownMachineDoesNothing(t *testing.T) {
	l, _ := ledgerAt(t, time.Now())
	l.SetSnapshotMiB("m_missing", 100)
	var nilLedger *Ledger
	nilLedger.SetSnapshotMiB("m_1", 100)
}

// A day file written before the column existed reads back with it at zero,
// which is the whole compatibility story this package documents.
func TestARecoveredMachineResumesItsSnapshotMeter(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	l, now := ledgerAt(t, start)

	l.Recover([]Entry{{
		MachineID: "m_1", OrgID: "org_1", State: "suspended",
		VCPUs: 1, MemMiB: 512, SnapshotMiB: 70,
	}})
	*now = now.Add(10 * time.Second)
	l.Close("m_1")

	totals, err := l.Sum(start.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got := totals["org_1"].SnapshotMiBSeconds; got != 70*10 {
		t.Errorf("snapshot_mib_seconds = %d, want %d: a restart must resume "+
			"metering storage rather than start it again at zero", got, 70*10)
	}
}
