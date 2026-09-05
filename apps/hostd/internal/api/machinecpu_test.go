package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A cold boot is a machine that came back with its URL, its disk and its
// volume and without its processes. From outside it is indistinguishable from
// a resume unless the API says so, and "indistinguishable from a bug" is what
// a silent reboot would be. So last_start is on every machine read.
func TestAMachineReportsHowItLastStarted(t *testing.T) {
	h, st, fake := newTestServerWithManager(t)
	ctx := context.Background()

	if err := st.PutMachineCPU(ctx, &state.MachineCPU{
		ID: fake.machine.ID, Kind: state.KindMachine, Vendor: "GenuineIntel",
		LastStart: state.StartColdBoot, LastStartAt: 1700000000,
	}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/v1/machines/"+fake.machine.ID, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	var got Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LastStart != "cold_boot" || got.LastStartAt != 1700000000 {
		t.Errorf("last_start = %q/%d, want cold_boot", got.LastStart, got.LastStartAt)
	}

	// And the list path reports it too, so a client watching a fleet sees the
	// downgrade without asking about each machine.
	rec = do(t, h, "GET", "/v1/machines", testKey)
	var list []Machine
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].LastStart != "cold_boot" {
		t.Errorf("list reported %+v", list)
	}
}

// A machine that predates machine_cpu has no recorded start. Both fields are
// omitempty, so it looks exactly as it did before this table existed rather
// than reporting an invented one.
func TestAMachineWithNoRecordedStartOmitsTheFields(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := do(t, h, "GET", "/v1/machines/"+fake.machine.ID, testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["last_start"]; ok {
		t.Errorf("an unrecorded machine reported last_start = %v", raw["last_start"])
	}
	if _, ok := raw["last_start_at"]; ok {
		t.Errorf("an unrecorded machine reported last_start_at = %v", raw["last_start_at"])
	}
}

// The fleet's vendor split has to be visible without a shell on every box: it
// is what says which of the fleet's snapshots a host can load, and therefore
// whether tier 2 exists for a given machine at all.
func TestHealthAndHostsReportTheCPUVendor(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.PutHost(ctx, &state.Host{ID: "host-a", LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHostCPU(ctx, &state.HostCPU{HostID: "host-a", Vendor: "AuthenticAMD"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutHost(ctx, &state.Host{ID: "host-b", LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testKey))
	if err := st.PutAPIKey(ctx, &state.APIKey{
		Hash: hex.EncodeToString(sum[:]), OrgID: "org_1", Scopes: "admin",
	}); err != nil {
		t.Fatal(err)
	}

	h := Routes(Deps{HostID: "host-a", Store: st, CPUVendor: "AuthenticAMD"})

	rec := do(t, h, "GET", "/v1/health", "")
	var health HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.CPUVendor != "AuthenticAMD" {
		t.Errorf("health cpu_vendor = %q", health.CPUVendor)
	}
	// Never set on a real host, so it must not appear on one.
	if health.CPUVendorForced {
		t.Error("health reported cpu_vendor_forced on a host with no fault armed")
	}

	rec = do(t, h, "GET", "/v1/hosts", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/hosts: %d %s", rec.Code, rec.Body)
	}
	var hosts []Host
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts", len(hosts))
	}
	if hosts[0].CPUVendor != "AuthenticAMD" {
		t.Errorf("host-a's cpu_vendor = %q", hosts[0].CPUVendor)
	}
	// A host that has not published its row yet is in no pool, and says so by
	// omission rather than by claiming one.
	if hosts[1].CPUVendor != "" {
		t.Errorf("host-b's cpu_vendor = %q, want empty", hosts[1].CPUVendor)
	}
}

// The fault flag announces itself, so the fleet gate can prove it armed the
// downgrade rather than assume it.
func TestHealthSaysWhenTheVendorIsForced(t *testing.T) {
	h := Routes(Deps{HostID: "host-a", CPUVendor: "GenuineIntel", CPUVendorForced: true})

	rec := do(t, h, "GET", "/v1/health", "")
	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CPUVendor != "GenuineIntel" || !got.CPUVendorForced {
		t.Errorf("health = %+v, want a forced GenuineIntel", got)
	}
}
