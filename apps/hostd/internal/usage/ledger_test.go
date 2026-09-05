package usage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is an injectable now. Every test drives time by hand: a meter whose
// tests wait for real seconds is a meter nobody runs.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// newLedger builds a ledger in a temp dir whose clock starts at noon UTC on a
// fixed day, far from a boundary unless a test moves it there.
func newLedger(t *testing.T) (*Ledger, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
	l := New(t.TempDir())
	l.now = c.now
	return l, c
}

// wide is a range that covers everything any test writes.
func wide() (int64, int64) {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix(),
		time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC).Unix()
}

func TestTwoOrgsAccrueSeparately(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	l.Open("m_2", "org_b", "running", 1, 512, 0)
	c.advance(90 * time.Second)
	l.Close("m_1")
	l.Close("m_2")

	since, until := wide()
	got, err := l.Sum(since, until)
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got["org_a"].MachineSeconds != 90 || got["org_b"].MachineSeconds != 90 {
		t.Errorf("machine_seconds = a:%d b:%d, want 90 each",
			got["org_a"].MachineSeconds, got["org_b"].MachineSeconds)
	}
}

// The billing fact: a suspended machine bills storage only.
func TestSuspendedBillsStorageOnly(t *testing.T) {
	l, c := newLedger(t)

	l.Open("m_run", "org_a", "running", 2, 1024, 10)
	c.advance(60 * time.Second)
	l.Close("m_run")

	l.Open("m_susp", "org_b", "suspended", 2, 1024, 10)
	c.advance(60 * time.Second)
	l.Close("m_susp")

	since, until := wide()
	got, _ := l.Sum(since, until)

	run := got["org_a"]
	if run.MachineSeconds != 60 || run.VCPUSeconds != 120 || run.MiBSeconds != 61440 {
		t.Errorf("running totals = %+v, want 60 machine, 120 vcpu, 61440 mib", run)
	}
	susp := got["org_b"]
	if susp.MachineSeconds != 60 {
		t.Errorf("suspended machine_seconds = %d, want 60", susp.MachineSeconds)
	}
	if susp.VCPUSeconds != 0 || susp.MiBSeconds != 0 {
		t.Errorf("suspended billed compute: %+v, want zero vcpu and mib", susp)
	}
	// Storage bills identically in both states -- that is the whole point.
	if run.VolumeGiBSeconds != 600 || susp.VolumeGiBSeconds != 600 {
		t.Errorf("volume_gib_seconds = running:%d suspended:%d, want 600 each",
			run.VolumeGiBSeconds, susp.VolumeGiBSeconds)
	}
}

// An error state still holds a row and a disk, so wall time and storage accrue
// and compute does not.
func TestErrorStateBillsLikeSuspended(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "error", 4, 2048, 5)
	c.advance(30 * time.Second)
	l.Close("m_1")

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 30 || got["org_a"].VCPUSeconds != 0 {
		t.Errorf("error totals = %+v, want 30 machine and no compute", got["org_a"])
	}
	if got["org_a"].VolumeGiBSeconds != 150 {
		t.Errorf("volume_gib_seconds = %d, want 150", got["org_a"].VolumeGiBSeconds)
	}
}

func TestAnIntervalCrossingMidnightSplitsIntoTwoDayFiles(t *testing.T) {
	l, c := newLedger(t)
	c.set(time.Date(2026, 9, 5, 23, 59, 30, 0, time.UTC))
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(60 * time.Second) // 00:00:30 the next day
	l.Close("m_1")

	for _, tc := range []struct{ day, want string }{
		{"2026-09-05", `"to":` + strconv.FormatInt(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).Unix(), 10)},
		{"2026-09-06", `"from":` + strconv.FormatInt(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).Unix(), 10)},
	} {
		raw, err := os.ReadFile(filepath.Join(l.dir, tc.day+".ndjson"))
		if err != nil {
			t.Fatalf("reading %s: %v", tc.day, err)
		}
		if n := len(nonEmpty(string(raw))); n != 1 {
			t.Errorf("%s has %d lines, want 1", tc.day, n)
		}
		if !strings.Contains(string(raw), tc.want) {
			t.Errorf("%s does not cut at midnight: %s", tc.day, raw)
		}
	}

	// Both halves are still billed, once each.
	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 60 {
		t.Errorf("machine_seconds across midnight = %d, want 60", got["org_a"].MachineSeconds)
	}
	// And each day answers for its own 30 seconds.
	day6 := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).Unix()
	got, _ = l.Sum(day6, day6+86400)
	if got["org_a"].MachineSeconds != 30 {
		t.Errorf("the new day's share = %d, want 30", got["org_a"].MachineSeconds)
	}
}

func TestSumClipsALineThatStraddlesBothEnds(t *testing.T) {
	l, c := newLedger(t)
	start := c.now().Unix()
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(100 * time.Second)
	l.Close("m_1")

	got, _ := l.Sum(start+10, start+40)
	if got["org_a"].MachineSeconds != 30 {
		t.Errorf("clipped machine_seconds = %d, want 30", got["org_a"].MachineSeconds)
	}
}

func TestAnOpenIntervalIsSummedToNow(t *testing.T) {
	l, c := newLedger(t)
	start := c.now().Unix()
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(45 * time.Second)

	// until in the future must not bill time nobody has spent.
	got, _ := l.Sum(start, start+10_000)
	if got["org_a"].MachineSeconds != 45 {
		t.Errorf("open interval summed to %d, want 45", got["org_a"].MachineSeconds)
	}
}

func TestTickPreservesTotalsAndDoublesTheLines(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(30 * time.Second)
	l.Tick()
	c.advance(30 * time.Second)
	l.Tick()
	c.advance(30 * time.Second)
	l.Close("m_1")

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 90 {
		t.Errorf("machine_seconds across two ticks = %d, want 90", got["org_a"].MachineSeconds)
	}
	if n := len(linesOn(t, l, "2026-09-05")); n != 3 {
		t.Errorf("%d lines, want one per tick plus the close", n)
	}
}

func TestReopeningAMachineClosesItsPreviousInterval(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(20 * time.Second)
	// A re-Open is a transition with new sizes.
	l.Open("m_1", "org_a", "running", 4, 2048, 0)
	c.advance(20 * time.Second)
	l.Close("m_1")

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 40 {
		t.Errorf("machine_seconds = %d, want 40 with no overlap", got["org_a"].MachineSeconds)
	}
	if want := int64(20*1 + 20*4); got["org_a"].VCPUSeconds != want {
		t.Errorf("vcpu_seconds = %d, want %d", got["org_a"].VCPUSeconds, want)
	}
}

func TestTransitionKeepsTheSizes(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "running", 2, 1024, 0)
	c.advance(10 * time.Second)
	l.Transition("m_1", "suspended")
	c.advance(10 * time.Second)
	l.Transition("m_1", "running")
	c.advance(10 * time.Second)
	l.Close("m_1")

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 30 {
		t.Errorf("machine_seconds = %d, want 30", got["org_a"].MachineSeconds)
	}
	// 20 running seconds at 2 vCPUs; the suspended 10 bill none.
	if got["org_a"].VCPUSeconds != 40 {
		t.Errorf("vcpu_seconds = %d, want 40", got["org_a"].VCPUSeconds)
	}
}

func TestNothingAccruesAfterClose(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(10 * time.Second)
	l.Close("m_1")
	c.advance(600 * time.Second)

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 10 {
		t.Errorf("machine_seconds after a destroy = %d, want 10", got["org_a"].MachineSeconds)
	}
}

func TestRecoverReopensOneIntervalPerEntry(t *testing.T) {
	l, c := newLedger(t)
	l.Recover([]Entry{
		{MachineID: "m_1", OrgID: "org_a", State: "running", VCPUs: 1, MemMiB: 512},
		{MachineID: "m_2", OrgID: "org_b", State: "suspended", VCPUs: 1, MemMiB: 512, VolumeGiB: 3},
	})
	c.advance(10 * time.Second)

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds != 10 || got["org_b"].MachineSeconds != 10 {
		t.Errorf("recovered totals = %+v", got)
	}
	if got["org_b"].VolumeGiBSeconds != 30 {
		t.Errorf("recovered volume_gib_seconds = %d, want 30", got["org_b"].VolumeGiBSeconds)
	}
}

// A row created before tenancy existed has no org. Its usage is reported under
// the empty string rather than dropped.
func TestAnOrglessMachineIsSummedUnderTheEmptyString(t *testing.T) {
	l, c := newLedger(t)
	l.Open("m_1", "", "running", 1, 512, 0)
	c.advance(10 * time.Second)
	l.Close("m_1")

	since, until := wide()
	got, _ := l.Sum(since, until)
	if got[""].MachineSeconds != 10 {
		t.Errorf("orgless machine_seconds = %d, want 10", got[""].MachineSeconds)
	}
}

func TestSumIsNeverNil(t *testing.T) {
	l, _ := newLedger(t)
	got, err := l.Sum(0, 1)
	if err != nil || got == nil {
		t.Fatalf("Sum on an empty ledger = %v, %v; want an empty map", got, err)
	}
}

// fakeUploader records the keys it was given.
type fakeUploader struct {
	mu   sync.Mutex
	keys []string
	fail bool
}

func (f *fakeUploader) PutFile(_ context.Context, key, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errFake
	}
	f.keys = append(f.keys, key)
	return nil
}

func (f *fakeUploader) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

var errFake = &uploadError{}

type uploadError struct{}

func (*uploadError) Error() string { return "upload refused" }

func TestUploadPutsYesterdayOnceAndTodayOnlyWhenItGrew(t *testing.T) {
	l, c := newLedger(t)
	up := &fakeUploader{}

	// A closed interval on the previous day, then the clock moves past
	// midnight so that day is final.
	c.set(time.Date(2026, 9, 5, 23, 0, 0, 0, time.UTC))
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(30 * time.Minute)
	l.Close("m_1")
	c.set(time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC))

	l.upload(context.Background(), up, "h1")
	if want := []string{"usage/h1/2026-09-05.ndjson"}; !equal(up.got(), want) {
		t.Fatalf("first upload = %v, want %v", up.got(), want)
	}
	if _, err := os.Stat(filepath.Join(l.dir, "2026-09-05.uploaded")); err != nil {
		t.Errorf("a finished day was not marked: %v", err)
	}

	// A second pass with nothing new must send nothing: the marker is what
	// stops a finished day being re-read forever.
	l.upload(context.Background(), up, "h1")
	if n := len(up.got()); n != 1 {
		t.Errorf("a finished day was uploaded %d times, want 1", n)
	}

	// Today's file, once it exists, is put on the tick it grew on.
	l.Open("m_2", "org_a", "running", 1, 512, 0)
	c.advance(time.Minute)
	l.Close("m_2")
	l.upload(context.Background(), up, "h1")
	if want := []string{"usage/h1/2026-09-05.ndjson", "usage/h1/2026-09-06.ndjson"}; !equal(up.got(), want) {
		t.Fatalf("after today grew = %v, want %v", up.got(), want)
	}
	// And not again while it has not.
	l.upload(context.Background(), up, "h1")
	if n := len(up.got()); n != 2 {
		t.Errorf("today was re-uploaded without growing: %v", up.got())
	}
	// Today is never marked done: it can still grow.
	if _, err := os.Stat(filepath.Join(l.dir, "2026-09-06.uploaded")); err == nil {
		t.Error("today was marked uploaded, so its later lines would never be sent")
	}
}

// A failed put must be retried, not silently dropped: object storage is the
// record.
func TestAFailedUploadIsRetriedOnTheNextTick(t *testing.T) {
	l, c := newLedger(t)
	up := &fakeUploader{fail: true}

	l.Open("m_1", "org_a", "running", 1, 512, 0)
	c.advance(time.Minute)
	l.Close("m_1")

	l.upload(context.Background(), up, "h1")
	if len(up.got()) != 0 {
		t.Fatalf("a failing uploader recorded %v", up.got())
	}
	up.mu.Lock()
	up.fail = false
	up.mu.Unlock()
	l.upload(context.Background(), up, "h1")
	if want := []string{"usage/h1/2026-09-05.ndjson"}; !equal(up.got(), want) {
		t.Errorf("retry = %v, want %v", up.got(), want)
	}
}

func TestRunTicksAndUploadsUntilTheContextEnds(t *testing.T) {
	l, c := newLedger(t)
	l.interval = time.Millisecond
	up := &fakeUploader{}

	l.Open("m_1", "org_a", "running", 1, 512, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx, up, "h1"); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(up.got()) == 0 && time.Now().Before(deadline) {
		c.advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
	if len(up.got()) == 0 {
		t.Fatal("Run uploaded nothing")
	}
	// Ticking wrote lines, so the totals are readable without a close.
	since, until := wide()
	got, _ := l.Sum(since, until)
	if got["org_a"].MachineSeconds == 0 {
		t.Error("Run ticked but nothing accrued")
	}
}

// Nil is the manager built without a ledger: a test manager, the fake. Every
// method has to be a no-op so no call site needs a check.
func TestEveryMethodIsNilSafe(t *testing.T) {
	var l *Ledger
	l.Open("m_1", "org_a", "running", 1, 512, 0)
	l.Transition("m_1", "suspended")
	l.Close("m_1")
	l.Tick()
	l.Recover([]Entry{{MachineID: "m_1"}})
	l.Run(context.Background(), nil, "h1")
	got, err := l.Sum(0, 1)
	if err != nil {
		t.Fatalf("Sum on a nil ledger: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("Sum on a nil ledger = %v, want an empty map", got)
	}
}

func linesOn(t *testing.T, l *Ledger, day string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(l.dir, day+".ndjson"))
	if err != nil {
		t.Fatalf("reading %s: %v", day, err)
	}
	return nonEmpty(string(raw))
}

func nonEmpty(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
