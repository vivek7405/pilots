package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
	"github.com/vivek7405/pilots/hostd/internal/usage"
)

// fakeUsage is a ledger that answers a fixed set and records what it was asked.
type fakeUsage struct {
	totals      map[string]usage.Totals
	byMachine   map[string]map[string]usage.Totals
	err         error
	since, unti int64
	// byMachineCalls counts the per-machine reads, so a test can assert the
	// larger answer is computed only when it was asked for.
	byMachineCalls int
}

func (f *fakeUsage) Sum(since, until int64) (map[string]usage.Totals, error) {
	f.since, f.unti = since, until
	return f.totals, f.err
}

func (f *fakeUsage) SumByMachine(since, until int64) (map[string]map[string]usage.Totals, error) {
	f.byMachineCalls++
	return f.byMachine, f.err
}

// usageServer is newTestServerWithManager plus a ledger.
func usageServer(t *testing.T, src UsageSource) (http.Handler, state.Store) {
	t.Helper()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	seedKey(t, st, testKey, "org_1", "admin")
	return Routes(Deps{HostID: "host-test", Store: st, Usage: src}), st
}

func TestUsageAnswersEveryOrgThisHostMetered(t *testing.T) {
	src := &fakeUsage{totals: map[string]usage.Totals{
		"org_a": {MachineSeconds: 90, VCPUSeconds: 180, MiBSeconds: 46080, VolumeGiBSeconds: 900},
		"org_b": {MachineSeconds: 60},
	}}
	h, _ := usageServer(t, src)

	rec := do(t, h, "GET", "/v1/usage?since=1000&until=2000", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HostID != "host-test" {
		t.Errorf("host_id = %q, want this host's -- the dashboard keys its "+
			"watermark on it", got.HostID)
	}
	// The range the host summed, echoed back: the poller advances its
	// watermark from the answer, not from what it asked for.
	if got.Since != 1000 || got.Until != 2000 {
		t.Errorf("range = [%d, %d), want [1000, 2000)", got.Since, got.Until)
	}
	if src.since != 1000 || src.unti != 2000 {
		t.Errorf("the ledger was asked for [%d, %d)", src.since, src.unti)
	}
	if len(got.Orgs) != 2 {
		t.Fatalf("orgs = %+v, want both", got.Orgs)
	}
	a := got.Orgs["org_a"]
	if a.MachineSeconds != 90 || a.VCPUSeconds != 180 || a.MiBSeconds != 46080 ||
		a.VolumeGiBSeconds != 900 {
		t.Errorf("org_a = %+v, want every unit carried through", a)
	}
}

func TestUsageDefaultsToTheLastDay(t *testing.T) {
	src := &fakeUsage{totals: map[string]usage.Totals{}}
	h, _ := usageServer(t, src)

	before := time.Now().Unix()
	rec := do(t, h, "GET", "/v1/usage", testKey)
	after := time.Now().Unix()
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var got UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Until < before || got.Until > after {
		t.Errorf("until = %d, want now (between %d and %d)", got.Until, before, after)
	}
	if got.Until-got.Since != defaultUsageWindow {
		t.Errorf("window = %d seconds, want %d", got.Until-got.Since, defaultUsageWindow)
	}
}

func TestUsageRefusesARangeItCannotSum(t *testing.T) {
	h, _ := usageServer(t, &fakeUsage{totals: map[string]usage.Totals{}})

	for _, tc := range []struct{ name, query, want string }{
		{"a non-integer since", "?since=x", "since"},
		{"a non-integer until", "?until=lastweek", "until"},
		{"an inverted range", "?since=10&until=5", "since must be before until"},
		{"an empty range", "?since=5&until=5", "since must be before until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, "GET", "/v1/usage"+tc.query, testKey)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body %s does not name the problem (%q)", rec.Body, tc.want)
			}
		})
	}
}

// A host with no ledger answers an empty SET of orgs, never a null one. The
// dashboard's poller tolerates null; the CSV export downstream of it does not,
// and the route staying in the table is what keeps a fleet's answers uniform.
func TestUsageWithNoLedgerAnswersAnEmptyMap(t *testing.T) {
	h, _ := usageServer(t, nil)
	rec := do(t, h, "GET", "/v1/usage", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"orgs":{}`) {
		t.Errorf("body = %s, want an empty orgs object", rec.Body)
	}
}

func TestUsageReportsALedgerItCannotRead(t *testing.T) {
	h, _ := usageServer(t, &fakeUsage{err: errors.New("usage: read /var/lib: permission denied")})
	rec := do(t, h, "GET", "/v1/usage", testKey)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", rec.Code, rec.Body)
	}
	// Reported rather than answered as zero. The poller only advances its
	// watermark on a successful answer, so a zero here would be billed as a
	// day in which nothing ran.
	if !strings.Contains(rec.Body.String(), "permission denied") {
		t.Errorf("body = %s, want the ledger's error", rec.Body)
	}
}

// The real ledger satisfies the interface. Nothing else proves the two halves
// of this feature are the same shape.
func TestALedgerIsAUsageSource(t *testing.T) {
	var _ UsageSource = usage.New(t.TempDir())
}

// An invoice line nobody can explain is the failure this answers. Fly
// aggregated under a payment provider's rate limit, shipped lines reading
// "9,972,448 seconds", and said afterwards it cost them a lot of trust.
func TestUsageByMachineBreaksTheOrgDown(t *testing.T) {
	src := &fakeUsage{
		totals: map[string]usage.Totals{
			"org_1": {MachineSeconds: 300, VCPUSeconds: 100},
		},
		byMachine: map[string]map[string]usage.Totals{
			"org_1": {
				"m_a": {MachineSeconds: 200, VCPUSeconds: 60, SnapshotMiBSeconds: 2048},
				"m_b": {MachineSeconds: 100, VCPUSeconds: 40},
			},
		},
	}
	h, _ := usageServer(t, src)

	rec := do(t, h, "GET", "/v1/usage?by=machine", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rows := got.Machines["org_1"]
	if len(rows) != 2 {
		t.Fatalf("machines = %+v, want two", rows)
	}
	if rows["m_a"].MachineSeconds != 200 || rows["m_b"].MachineSeconds != 100 {
		t.Errorf("per-machine seconds = %+v", rows)
	}
	// The org total is still there: the breakdown explains the line, it does
	// not replace it.
	if got.Orgs["org_1"].MachineSeconds != 300 {
		t.Errorf("org total = %+v, want it alongside the breakdown", got.Orgs["org_1"])
	}
	// And the snapshot dimension converts to GiB the same way.
	if rows["m_a"].SnapshotGiBSeconds != 2 {
		t.Errorf("snapshot_gib_seconds = %d, want 2", rows["m_a"].SnapshotGiBSeconds)
	}
}

// The larger answer is computed only when asked for: most callers want the
// invoice line rather than its derivation.
func TestUsageWithoutByMachineDoesNotComputeIt(t *testing.T) {
	src := &fakeUsage{totals: map[string]usage.Totals{"org_1": {MachineSeconds: 10}}}
	h, _ := usageServer(t, src)

	rec := do(t, h, "GET", "/v1/usage", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if src.byMachineCalls != 0 {
		t.Errorf("the per-machine sum ran %d times for a request that did not ask",
			src.byMachineCalls)
	}
	var got UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Machines != nil {
		t.Errorf("machines = %+v on a request that did not ask", got.Machines)
	}
}
