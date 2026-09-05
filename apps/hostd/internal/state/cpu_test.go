package state

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func amd(id string) Host   { return Host{ID: id, Vendor: "AuthenticAMD"} }
func intel(id string) Host { return Host{ID: id, Vendor: "GenuineIntel"} }

// A memory image restores only on the vendor that photographed it, so the
// ranking has to land on that pool while any host of it is alive. This is
// tier 2, and it is the whole reason MachineOwnerFor exists.
func TestMachineOwnerForRanksTheVendorPoolFirst(t *testing.T) {
	live := []Host{amd("host-a"), amd("host-b"), intel("host-c")}
	pool := map[string]bool{"host-a": true, "host-b": true}

	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("m_%d", i)
		owner, ok := MachineOwnerFor(id, "AuthenticAMD", live)
		if !ok {
			t.Fatalf("%s: no owner on a three-host fleet", id)
		}
		if !pool[owner] {
			t.Fatalf("%s: an AMD image was ranked onto %s, which cannot restore it", id, owner)
		}
	}
}

// Tier 3: with no host of the image's pool alive, availability wins over
// continuity and the whole live set is ranked. The answer must be OwnerFor's
// exactly, so every host computes the same one, and it must not be "no owner"
// -- that would leave the machine unrescued forever.
func TestMachineOwnerForFallsBackToTheWholeFleet(t *testing.T) {
	live := []Host{intel("host-c"), intel("host-d")}

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("m_%d", i)
		got, ok := MachineOwnerFor(id, "AuthenticAMD", live)
		if !ok {
			t.Fatalf("%s: an image with no live pool was left unrescued", id)
		}
		want, _ := OwnerFor(id, live)
		if got != want {
			t.Fatalf("%s: fell back to %s, not OwnerFor's %s", id, got, want)
		}
	}
}

// Rank is a position in a list. Two hosts that see the same membership in a
// different order must still compute the same owner, or they either rescue the
// same machine twice or neither rescues it.
func TestMachineOwnerForIsStableAcrossHostOrder(t *testing.T) {
	live := []Host{amd("host-a"), intel("host-b"), amd("host-c"), intel("host-d")}

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("m_%d", i)
		want, _ := MachineOwnerFor(id, "AuthenticAMD", live)

		shuffled := append([]Host(nil), live...)
		rng.Shuffle(len(shuffled), func(a, b int) {
			shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
		})
		got, _ := MachineOwnerFor(id, "AuthenticAMD", shuffled)
		if got != want {
			t.Fatalf("%s: %s from one order and %s from another", id, want, got)
		}
	}
}

// A machine that predates machine_cpu has no recorded pool. It must rank over
// the whole fleet, which is what it did before this table existed: an absent
// row is not a reason to change an existing machine's behaviour.
func TestAnUnknownVendorRanksOverTheWholeFleet(t *testing.T) {
	live := []Host{amd("host-a"), intel("host-b"), amd("host-c")}

	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("m_%d", i)
		got, ok := MachineOwnerFor(id, "", live)
		want, wantOK := OwnerFor(id, live)
		if got != want || ok != wantOK {
			t.Fatalf("%s: unknown vendor gave %s/%v, OwnerFor gives %s/%v", id, got, ok, want, wantOK)
		}
	}
}

// The row writers must NOT be vendor filtered. A service, a domain and a repo
// delivery have one arbiter each and no host_id column to guard them; splitting
// the candidate set by vendor would give two hosts the same row and the merge
// would corrupt it silently. Only the MACHINE ranking narrows.
func TestServiceArbiterIsNotVendorFiltered(t *testing.T) {
	plain := []Host{{ID: "host-a"}, {ID: "host-b"}, {ID: "host-c"}}
	mixed := []Host{amd("host-a"), intel("host-b"), amd("host-c")}

	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("svc_%d", i)
		before, _ := OwnerFor(id, plain)
		after, _ := OwnerFor(id, mixed)
		if before != after {
			t.Fatalf("%s: the arbiter moved from %s to %s when the hosts gained vendors", id, before, after)
		}
	}

	// And the id minter still mints ids this host owns on a mixed fleet.
	for _, self := range []string{"host-a", "host-b", "host-c"} {
		id := NewOwnedID("svc_", self, mixed)
		owner, _ := OwnerFor(id, mixed)
		if owner != self {
			t.Fatalf("%s minted %s, which %s owns", self, id, owner)
		}
	}
}

func TestHostCPUAndMachineCPURoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &HostCPU{HostID: "host-a", Vendor: "AuthenticAMD", CPUTemplate: "T2A", UpdatedAt: 1700000000}
	if err := s.PutHostCPU(ctx, want); err != nil {
		t.Fatalf("PutHostCPU: %v", err)
	}
	rows, err := s.ListHostCPU(ctx)
	if err != nil {
		t.Fatalf("ListHostCPU: %v", err)
	}
	if len(rows) != 1 || rows[0] != *want {
		t.Fatalf("ListHostCPU = %+v, want one %+v", rows, *want)
	}

	if _, err := s.GetMachineCPU(ctx, "m_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMachineCPU on an unrecorded machine = %v, want ErrNotFound", err)
	}

	cpu := &MachineCPU{
		ID: "m_1", Kind: KindMachine, Vendor: "GenuineIntel",
		LastStart: StartColdBoot, LastStartAt: 1700000001, UpdatedAt: 1700000001,
	}
	if err := s.PutMachineCPU(ctx, cpu); err != nil {
		t.Fatalf("PutMachineCPU: %v", err)
	}
	got, err := s.GetMachineCPU(ctx, "m_1")
	if err != nil {
		t.Fatalf("GetMachineCPU: %v", err)
	}
	if *got != *cpu {
		t.Fatalf("GetMachineCPU = %+v, want %+v", *got, *cpu)
	}

	// The row is rewritten on every start, so an update must land.
	cpu.LastStart, cpu.LastStartAt = StartRestore, 1700000002
	if err := s.PutMachineCPU(ctx, cpu); err != nil {
		t.Fatalf("PutMachineCPU again: %v", err)
	}
	if got, _ = s.GetMachineCPU(ctx, "m_1"); got.LastStart != StartRestore {
		t.Fatalf("last_start stayed %q after a second write", got.LastStart)
	}
}

// The vendor reaches the ranking through ListHosts, so the join is what makes
// tier 2 possible at all. A host with no cpu row reads as empty, not as an
// error: it is a host that has not finished starting.
func TestListHostsCarriesTheVendor(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, id := range []string{"host-a", "host-b"} {
		if err := s.PutHost(ctx, &Host{ID: id, WGAddr: "fdcc::1", LastSeen: 1}); err != nil {
			t.Fatalf("PutHost %s: %v", id, err)
		}
	}
	if err := s.PutHostCPU(ctx, &HostCPU{HostID: "host-a", Vendor: "AuthenticAMD"}); err != nil {
		t.Fatalf("PutHostCPU: %v", err)
	}

	hosts, err := s.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("ListHosts returned %d hosts", len(hosts))
	}
	if hosts[0].Vendor != "AuthenticAMD" {
		t.Errorf("host-a's vendor is %q, want AuthenticAMD", hosts[0].Vendor)
	}
	if hosts[1].Vendor != "" {
		t.Errorf("host-b has no cpu row; its vendor is %q, want empty", hosts[1].Vendor)
	}
}

// One row per vendor pool, because the template's memory half is a Firecracker
// snapshot and never restores across the boundary.
func TestGoldenTemplateIsPerVendorPool(t *testing.T) {
	if GoldenTemplateFor("AuthenticAMD") == GoldenTemplateFor("GenuineIntel") {
		t.Fatal("both vendor pools name the same template row")
	}
	if got := GoldenTemplateFor("AuthenticAMD"); got != "golden-AuthenticAMD" {
		t.Errorf("GoldenTemplateFor = %q", got)
	}
}
