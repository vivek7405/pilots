package netns

import (
	"fmt"
	"testing"
)

// The high byte of idx lands in the third octet. Formatting these as
// "10.11.0.%d" is correct for idx < 256 and then silently collides -- slot 256
// would alias slot 0. These cases pin the boundaries.
func TestSlotAddressDerivation(t *testing.T) {
	for _, tc := range []struct {
		idx                     int
		hostIP, vethIP, vpeerIP string
	}{
		{1, "10.11.0.1", "10.12.0.2", "10.12.0.3"},
		{2, "10.11.0.2", "10.12.0.4", "10.12.0.5"},
		{127, "10.11.0.127", "10.12.0.254", "10.12.0.255"},
		// veth crosses into the third octet well before the host IP does.
		{128, "10.11.0.128", "10.12.1.0", "10.12.1.1"},
		{255, "10.11.0.255", "10.12.1.254", "10.12.1.255"},
		// The case a "10.11.0.%d" format string gets wrong.
		{256, "10.11.1.0", "10.12.2.0", "10.12.2.1"},
		{511, "10.11.1.255", "10.12.3.254", "10.12.3.255"},
		{512, "10.11.2.0", "10.12.4.0", "10.12.4.1"},
		{1023, "10.11.3.255", "10.12.7.254", "10.12.7.255"},
	} {
		t.Run(fmt.Sprintf("idx=%d", tc.idx), func(t *testing.T) {
			s := slotForIdx(tc.idx, "m")
			if got := s.HostIP.String(); got != tc.hostIP {
				t.Errorf("HostIP = %s, want %s", got, tc.hostIP)
			}
			if got := s.VEthIP.String(); got != tc.vethIP {
				t.Errorf("VEthIP = %s, want %s", got, tc.vethIP)
			}
			if got := s.VPeerIP.String(); got != tc.vpeerIP {
				t.Errorf("VPeerIP = %s, want %s", got, tc.vpeerIP)
			}
		})
	}
}

// Distinct indices must never collide on any address, across the whole pool.
func TestSlotAddressesAreUniqueAcrossPool(t *testing.T) {
	seen := map[string]int{}
	for idx := 1; idx < DefaultPoolSize; idx++ {
		s := slotForIdx(idx, "m")
		for _, addr := range []string{s.HostIP.String(), s.VEthIP.String(), s.VPeerIP.String()} {
			if prev, dup := seen[addr]; dup {
				t.Fatalf("address %s used by both slot %d and slot %d", addr, prev, idx)
			}
			seen[addr] = idx
		}
	}
}

// Every guest sees the same addresses regardless of slot -- this is what makes
// snapshots host-agnostic, so it is worth asserting rather than assuming.
func TestGuestFacingAddressesAreSlotIndependent(t *testing.T) {
	for _, idx := range []int{1, 42, 512, 1023} {
		s := slotForIdx(idx, "m")
		if s.VPeerName != "eth0" {
			t.Errorf("idx %d: VPeerName = %q, want eth0", idx, s.VPeerName)
		}
	}
	if TapGuestIP != "169.254.0.21" || TapHostIP != "169.254.0.22" {
		t.Fatal("guest-facing constants changed: this invalidates every snapshot in the fleet")
	}
}

func TestPoolTakeSkipsZeroAndAdvances(t *testing.T) {
	p := NewPool(8)
	first, err := p.Take("m1")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if first.Idx == 0 {
		t.Fatal("Take handed out index 0, the unallocated sentinel")
	}
	second, err := p.Take("m2")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if second.Idx == first.Idx {
		t.Fatalf("Take returned index %d twice", first.Idx)
	}
	if p.InUse() != 2 {
		t.Errorf("InUse = %d, want 2", p.InUse())
	}
}

func TestPoolExhaustionAndReuse(t *testing.T) {
	p := NewPool(4) // indices 1..3
	var taken []*Slot
	for i := 0; i < 3; i++ {
		s, err := p.Take(fmt.Sprintf("m%d", i))
		if err != nil {
			t.Fatalf("Take %d: %v", i, err)
		}
		taken = append(taken, s)
	}
	if _, err := p.Take("overflow"); err != ErrPoolFull {
		t.Fatalf("got %v, want ErrPoolFull", err)
	}

	p.Return(taken[1].Idx)
	reused, err := p.Take("m-new")
	if err != nil {
		t.Fatalf("Take after Return: %v", err)
	}
	if reused.Idx != taken[1].Idx {
		t.Errorf("reused idx = %d, want the returned %d", reused.Idx, taken[1].Idx)
	}
}

// Reserve is how a restarted hostd re-claims the slots of machines that kept
// running. It must be idempotent, because reconcile can legitimately run
// against a pool that already knows about the slot.
func TestPoolReserveIsIdempotent(t *testing.T) {
	p := NewPool(16)
	s1, err := p.Reserve(7, "m1")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	s2, err := p.Reserve(7, "m1")
	if err != nil {
		t.Fatalf("second Reserve for the same machine: %v", err)
	}
	if s1.Idx != s2.Idx || s1.HostIP.String() != s2.HostIP.String() {
		t.Error("Reserve is not deterministic for the same index")
	}
	if p.InUse() != 1 {
		t.Errorf("InUse = %d, want 1 after two Reserves of one slot", p.InUse())
	}
}

func TestPoolReserveRejectsConflictAndOutOfRange(t *testing.T) {
	p := NewPool(16)
	if _, err := p.Reserve(7, "m1"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := p.Reserve(7, "m2"); err == nil {
		t.Error("Reserve let a second machine claim a held slot")
	}
	for _, idx := range []int{0, -1, 16, 99} {
		if _, err := p.Reserve(idx, "m"); err == nil {
			t.Errorf("Reserve(%d) succeeded, want out-of-range error", idx)
		}
	}
}

// Reserve must not hand out an index Take would also hand out.
func TestReserveThenTakeDoNotCollide(t *testing.T) {
	p := NewPool(8)
	reserved, err := p.Reserve(3, "reconciled")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for i := 0; i < 6; i++ {
		s, err := p.Take(fmt.Sprintf("m%d", i))
		if err != nil {
			break
		}
		if s.Idx == reserved.Idx {
			t.Fatalf("Take handed out reserved slot %d", reserved.Idx)
		}
	}
}
