package main

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/config"
	"github.com/vivek7405/pilots/hostd/internal/mesh"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A /proc/meminfo from a host with a large hugepage pool reserved. The point
// is the relationship between the numbers: MemAvailable is small precisely
// BECAUSE the pool is big, since reserved hugepages are subtracted from it.
const hugePageMeminfo = `MemTotal:       98213396 kB
MemFree:         1234567 kB
MemAvailable:    2001000 kB
Buffers:          123456 kB
Cached:          4567890 kB
HugePages_Total:   40247
HugePages_Free:    40000
HugePages_Rsvd:        0
Hugepagesize:       2048 kB
`

const smallPageMeminfo = `MemTotal:       98213396 kB
MemFree:        50000000 kB
MemAvailable:   80000000 kB
HugePages_Total:       0
HugePages_Free:        0
Hugepagesize:       2048 kB
`

func TestMeminfoKeyReadsTheValueInItsOwnUnit(t *testing.T) {
	raw := []byte(hugePageMeminfo)
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"MemAvailable", 2001000},
		{"HugePages_Free", 40000},
		{"Hugepagesize", 2048},
		{"MemTotal", 98213396},
	} {
		if got := meminfoKey(raw, tc.key); got != tc.want {
			t.Errorf("meminfoKey(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}
}

// A key this kernel does not publish reads as zero, which every caller here
// treats as "no capacity" -- the safe direction for admission.
func TestMeminfoKeyReportsZeroForAMissingKey(t *testing.T) {
	if got := meminfoKey([]byte(smallPageMeminfo), "DirectMap1G"); got != 0 {
		t.Errorf("missing key read as %d, want 0", got)
	}
}

// HugePages_Rsvd shares a prefix with HugePages_Free but is a different key,
// and Sscanf matching loosely would return whichever came first.
func TestMeminfoKeyDoesNotConfuseSimilarKeys(t *testing.T) {
	if got := meminfoKey([]byte(hugePageMeminfo), "HugePages_Total"); got != 40247 {
		t.Errorf("HugePages_Total = %d, want 40247", got)
	}
	if got := meminfoKey([]byte(hugePageMeminfo), "HugePages_Rsvd"); got != 0 {
		t.Errorf("HugePages_Rsvd = %d, want 0", got)
	}
}

// This is the bug the change exists to prevent. Reserved hugepages are
// subtracted from MemAvailable outright, so a host whose guests all come out
// of the pool would advertise ~2 GiB free out of 98, look nearly full to the
// whole fleet, and refuse every self-heal rescue -- with nothing naming why.
func TestFreeMemCountsThePoolUnderHugePages(t *testing.T) {
	raw := []byte(hugePageMeminfo)

	fromPool := meminfoKey(raw, "HugePages_Free") * meminfoKey(raw, "Hugepagesize") / 1024
	fromAvailable := meminfoKey(raw, "MemAvailable") / 1024

	if fromPool != 80000 {
		t.Errorf("pool reports %d MiB, want 80000", fromPool)
	}
	if fromAvailable >= fromPool {
		t.Fatalf("fixture is wrong: MemAvailable %d MiB is not smaller than the "+
			"pool's %d MiB, so it cannot show the bug", fromAvailable, fromPool)
	}
	// 40x apart on this fixture. A host that answered with the smaller number
	// would refuse to rescue a 4 GiB machine it has 78 GiB of room for.
	if fromPool/fromAvailable < 10 {
		t.Errorf("pool/available = %d, want the fixture to keep them far apart",
			fromPool/fromAvailable)
	}
}

// A host with no mesh identity publishes an empty address rather than the one
// the zero key derives, which every such host would share.
func TestHeartbeatRowWithoutAMesh(t *testing.T) {
	cfg := &config.Config{HostID: "host-a", PublicIP: "203.0.113.7"}

	h := heartbeatFor(cfg, mesh.Keys{}, false)()
	if h.ID != "host-a" {
		t.Errorf("ID = %q, want host-a", h.ID)
	}
	if h.PublicIP != "203.0.113.7" {
		t.Errorf("PublicIP = %q, want 203.0.113.7", h.PublicIP)
	}
	if h.CPUFree != runtime.NumCPU() {
		t.Errorf("CPUFree = %d, want %d", h.CPUFree, runtime.NumCPU())
	}
	if h.WGAddr != "" || h.WGPubKey != "" {
		t.Errorf("keyless host published wg_addr %q and wg_pubkey %q, want both empty", h.WGAddr, h.WGPubKey)
	}

	keys, err := mesh.NewKeys()
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	meshed := heartbeatFor(cfg, keys, true)()
	if meshed.WGAddr != keys.Address().String() {
		t.Errorf("wg_addr = %q, want %q", meshed.WGAddr, keys.Address().String())
	}
	if meshed.WGPubKey != keys.Public.String() {
		t.Errorf("wg_pubkey = %q, want %q", meshed.WGPubKey, keys.Public.String())
	}
}

// The bug this fixes: a single box wrote no hosts row at all, so GET /v1/hosts
// answered [] on the exact setup the local runbook builds. Gating the
// heartbeat on cfg.Fleet() again leaves the list empty here.
func TestASingleBoxListsItself(t *testing.T) {
	store, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startHeartbeat(ctx, &config.Config{HostID: "host-a"}, store, mesh.Keys{}, false, nil)

	deadline := time.Now().Add(2 * time.Second)
	for {
		hosts, err := store.ListHosts(ctx)
		if err != nil {
			t.Fatalf("ListHosts: %v", err)
		}
		if len(hosts) == 1 {
			if hosts[0].ID != "host-a" {
				t.Fatalf("listed host %q, want host-a", hosts[0].ID)
			}
			if age := time.Since(time.Unix(hosts[0].LastSeen, 0)); age > time.Minute {
				t.Fatalf("last_seen is %v old, want a fresh heartbeat", age)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 2s the store lists %d hosts, want 1", len(hosts))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
