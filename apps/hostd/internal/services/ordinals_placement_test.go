package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pilotsrun/pilots/hostd/internal/api"
	"github.com/pilotsrun/pilots/hostd/internal/state"
)

// liveFleet puts three heartbeating hosts in the store.
func liveFleet(t *testing.T, store state.Store, ids ...string) {
	t.Helper()
	now := time.Now().Unix()
	for _, id := range ids {
		if err := store.PutHost(context.Background(), &state.Host{
			ID: id, WGAddr: "10.0.0.1", LastSeen: now,
			CPUFree: 32, MemFreeMiB: 65536,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Ordinals are placed on distinct hosts, and the placement is actually read.
//
// ordinalHost and OrdinalHostsFor existed and were correct; the file comment
// above them explained at length why ordinals spread across hosts; and the
// only caller of either was their own test. rollOutOrdinal never asked. Every
// ordinal's volume and every ordinal's machine were created on whichever host
// happened to be running the rollout, so `pilot db ha` built a three-node
// cluster on one box while printing a confirmation promising each node would
// be "placed on a different host where the fleet allows".
//
// That is the whole reason somebody buys a high-availability cluster: three
// members on one host are three members that die together. Reverting
// rollOutOrdinal to ignore ordinalPlacement collapses these three answers to
// this host.
func TestEachOrdinalIsPlacedOnItsOwnHost(t *testing.T) {
	m, _, store, _ := fixture(t, 3)
	liveFleet(t, store, "host-a", "host-b", "host-c")

	seen := map[string]bool{}
	for ordinal := 1; ordinal <= 3; ordinal++ {
		host := m.ordinalPlacement(context.Background(), "svc-1", ordinal)
		if host == "" {
			t.Fatalf("ordinal %d was placed nowhere", ordinal)
		}
		if seen[host] {
			t.Errorf("ordinal %d was placed on %s again; members of one cluster on "+
				"one host are members that die together", ordinal, host)
		}
		seen[host] = true
	}
}

// A host list this host cannot read is NOT a placement. Guessing from a
// partial view would put two members of one cluster on one box while telling
// the operator they were spread, which is worse than a cluster that is
// honestly local.
func TestAnUnreadableFleetPlacesNothing(t *testing.T) {
	m, _, _, _ := fixture(t, 3)
	// No hosts written at all, which is what a replica with nothing in it
	// looks like as well as a single box.
	if got := m.ordinalPlacement(context.Background(), "svc-1", 2); got != "" {
		t.Errorf("placement = %q with no live hosts, want none", got)
	}
}

// The volume follows the placement. A volume has exactly one writer on exactly
// one host, so creating it here and placing the replica elsewhere would be a
// cluster whose members cannot reach their own data.
func TestAnOrdinalsVolumeIsCreatedOnItsOwnHost(t *testing.T) {
	m, fm, store, svc := fixture(t, 3)
	liveFleet(t, store, "host-a", "host-b", "host-c")
	peers := &fakePeers{}
	m.opts.Peers = peers

	id, err := m.ensureOrdinalVolume(context.Background(), svc, 2, "host-c")
	if err != nil {
		t.Fatalf("ensureOrdinalVolume: %v", err)
	}
	if id == "" {
		t.Fatal("no volume id")
	}
	if len(peers.postJSONs) != 1 || !strings.HasPrefix(peers.postJSONs[0], "host-c /v1/volumes") {
		t.Errorf("peer calls = %v, want one create on host-c", peers.postJSONs)
	}
	for _, e := range fm.events {
		if strings.HasPrefix(e, "volume:") {
			t.Errorf("a volume placed on host-c was created locally: %v", fm.events)
		}
	}
}

// A replica placed on another host is created THERE, and the rollout learns
// the id the far side assigned -- which is what it needs to gate on health and
// to redeploy the ordinal next time.
func TestAPlacedReplicaIsCreatedOnItsHost(t *testing.T) {
	m, fm, store, svc := fixture(t, 3)
	liveFleet(t, store, "host-a", "host-b", "host-c")
	peers := &fakePeers{}
	m.opts.Peers = peers

	rel := &state.Release{ID: "rel-1", ServiceID: svc.ID, RootfsBuildID: "rootfs-1"}
	got, err := m.createReplicaOn(context.Background(), svc, rel, nil, "vol-2", "host-b")
	if err != nil {
		t.Fatalf("createReplicaOn: %v", err)
	}
	if got.ID == "" {
		t.Error("the rollout did not learn the id the far side assigned")
	}
	if got.HostID != "host-b" {
		t.Errorf("machine landed on %q, want host-b", got.HostID)
	}
	if len(peers.postJSONs) != 1 || !strings.HasPrefix(peers.postJSONs[0], "host-b /v1/machines") {
		t.Errorf("peer calls = %v, want one create on host-b", peers.postJSONs)
	}
	for _, e := range fm.events {
		if strings.HasPrefix(e, "create:") {
			t.Errorf("a replica placed on host-b was created locally: %v", fm.events)
		}
	}
}

// A placement naming this host is the ordinary local path, with no peer call
// and nothing about a single box changed.
func TestAPlacementOnThisHostStaysLocal(t *testing.T) {
	m, _, store, svc := fixture(t, 1)
	liveFleet(t, store, "host-a")
	peers := &fakePeers{}
	m.opts.Peers = peers

	if _, err := m.ensureOrdinalVolume(context.Background(), svc, 1, "host-a"); err != nil {
		t.Fatalf("ensureOrdinalVolume: %v", err)
	}
	if len(peers.postJSONs) != 0 {
		t.Errorf("a local placement made peer calls: %v", peers.postJSONs)
	}
}

// A placement this host cannot reach is refused, not silently served locally.
//
// Creating it here anyway is the failure this whole change exists to fix: a
// cluster that reports success and has two members on one box. The operator
// gets an error naming the host instead.
func TestAnUnreachablePlacementIsRefused(t *testing.T) {
	m, _, store, svc := fixture(t, 3)
	liveFleet(t, store, "host-a", "host-b", "host-c")
	m.opts.Peers = nil // no fleet client

	_, err := m.createVolumeOn(context.Background(),
		api.CreateVolumeRequest{Name: "web-2", SizeGiB: 1, MountPath: "/data"}, "host-c")
	if err == nil {
		t.Fatal("a volume for host-c was created here")
	}
	if !strings.Contains(err.Error(), "host-c") {
		t.Errorf("the refusal does not name the host: %v", err)
	}

	rel := &state.Release{ID: "rel-1", ServiceID: svc.ID, RootfsBuildID: "rootfs-1"}
	if _, err := m.createReplicaOn(context.Background(), svc, rel, nil, "vol-2", "host-c"); err == nil {
		t.Error("a replica for host-c was created here")
	}
}

// An ordinal's volume belongs to the service's org, wherever it was created:
// the org is never sent to a peer, so without this the volume was owned by
// nobody and invisible to the org whose database lives on it.
func TestAnOrdinalsVolumeIsOwnedByTheServicesOrg(t *testing.T) {
	m, _, store, svc := fixture(t, 3)
	ctx := context.Background()
	if err := store.PutTenancy(ctx, &state.Tenancy{ID: svc.ID, OrgID: "org_db", Kind: "service"}); err != nil {
		t.Fatal(err)
	}
	liveFleet(t, store, "host-a", "host-b", "host-c")
	m.opts.Peers = &fakePeers{}

	for _, host := range []string{"host-a", "host-c"} {
		ordinal := 2
		if host == "host-a" {
			ordinal = 1
		}
		id, err := m.ensureOrdinalVolume(ctx, svc, ordinal, host)
		if err != nil {
			t.Fatalf("ensureOrdinalVolume on %s: %v", host, err)
		}
		tn, err := store.GetTenancy(ctx, id)
		if err != nil || tn.OrgID != "org_db" {
			t.Errorf("volume %s created on %s: tenancy %+v, %v; want org_db", id, host, tn, err)
		}
	}
}

// Scaling an ordinal service down deletes the volumes above the new count,
// not only their machines and bindings: a detached volume left behind is a
// whole database copy in object storage that nothing can reach.
func TestPruningOrdinalsDeletesTheirVolumes(t *testing.T) {
	m, fm, store, svc := fixture(t, 3)
	ctx := context.Background()
	for ordinal, vol := range map[int]string{1: "vol_a", 2: "vol_b", 3: "vol_c"} {
		if err := store.PutServiceVolume(ctx, &state.ServiceVolume{ServiceID: svc.ID, Ordinal: ordinal, VolumeID: vol}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.pruneOrdinals(ctx, svc, 1); err != nil {
		t.Fatalf("pruneOrdinals: %v", err)
	}
	var deleted []string
	for _, e := range fm.events {
		if strings.HasPrefix(e, "delete-volume ") {
			deleted = append(deleted, strings.TrimPrefix(e, "delete-volume "))
		}
	}
	if strings.Join(deleted, ",") != "vol_c,vol_b" {
		t.Errorf("deleted volumes %v, want vol_c then vol_b", deleted)
	}
	left, err := store.ListServiceVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].VolumeID != "vol_a" {
		t.Errorf("bindings left %v, want only ordinal 1", left)
	}
}
