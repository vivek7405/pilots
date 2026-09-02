package corrosion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func volume(id, hostID string) *state.Volume {
	return &state.Volume{
		ID: id, Name: id, SizeMiB: 1024, S3Prefix: "volumes/" + id + "/",
		MountPath: "/data", HostID: hostID, CreatedAt: time.Now().Unix(),
	}
}

// Two hosts owning one volume is not a bookkeeping problem. Both mount the
// same SQLite metadata database, and the volume stops existing.
func TestPutVolumeRefusesAnotherHostsVolume(t *testing.T) {
	store, agent := newTestStore(t, "host-a")
	agent.exec(t, `INSERT INTO volumes (id, host_id) VALUES ('vol-1','host-b')`)

	err := store.PutVolume(context.Background(), volume("vol-1", "host-a"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("PutVolume on another host's volume returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM volumes WHERE id='vol-1'`); got != "host-b" {
		t.Fatalf("host_id = %q; the refused write landed anyway", got)
	}
}

// The rescue path: the owner is gone, so its volume can move here with it.
func TestPutVolumeClaimsFromADeadOwner(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO volumes (id, host_id) VALUES ('vol-1','host-a')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-a', 0)`)

	if err := store.PutVolume(ctx, volume("vol-1", "host-b"),
		state.WithDeadOwnerClaim("host-a")); err != nil {
		t.Fatalf("PutVolume claiming from a dead owner: %v", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM volumes WHERE id='vol-1'`); got != "host-b" {
		t.Fatalf("host_id = %q; the claim did not land", got)
	}
}

// Liveness is re-read at the write, not trusted from the caller's tick. The
// gap between a rescue loop deciding and writing is exactly where a host comes
// back -- and a volume claimed from a host that still has it mounted is two
// writers on one metadata database.
func TestPutVolumeRefusesToClaimFromALiveOwner(t *testing.T) {
	ctx := context.Background()
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO volumes (id, host_id) VALUES ('vol-1','host-a')`)
	agent.exec(t, `INSERT INTO hosts (id, last_seen) VALUES ('host-a', ?)`, time.Now().Unix())

	err := store.PutVolume(ctx, volume("vol-1", "host-b"), state.WithDeadOwnerClaim("host-a"))
	if !errors.Is(err, state.ErrNotOwner) {
		t.Fatalf("claiming from a heartbeating host returned %v, want ErrNotOwner", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM volumes WHERE id='vol-1'`); got != "host-a" {
		t.Fatalf("host_id = %q; the refused claim landed anyway", got)
	}
}

func TestVolumeRoundTripThroughTheAgent(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t, "host-a")

	want := volume("vol-1", "host-a")
	want.MachineID = "m-1"
	if err := store.PutVolume(ctx, want); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}
	got, err := store.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip changed the row:\n got %+v\nwant %+v", *got, *want)
	}
	list, err := store.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(list) != 1 || list[0].ID != "vol-1" {
		t.Fatalf("ListVolumes returned %+v", list)
	}
}

// A released volume must be claimable again, by any host.
//
// releaseVolume clears the row's owner, and the guard used to admit only the
// host already named in it -- so a row reading host_id='' matched nobody and
// the volume could never be picked up again. The way it failed hid it: while
// release left host_id naming the old host, every other host took the rescue
// branch instead, asked to claim the volume from a host that was alive and
// heartbeating, and was correctly refused. Permanently. A volume detached
// cleanly on one host could never be attached on another.
func TestAnUnownedVolumeCanBeClaimed(t *testing.T) {
	store, agent := newTestStore(t, "host-b")
	agent.exec(t, `INSERT INTO volumes (id, host_id) VALUES ('vol-1','')`)

	// No dead-owner claim: nobody owns it. That is the whole point -- the
	// releasing host may well still be alive, and asking to take it from a
	// live host is exactly what used to be refused.
	if err := store.PutVolume(context.Background(), volume("vol-1", "host-b")); err != nil {
		t.Fatalf("claiming an unowned volume: %v", err)
	}
	if got := agent.scalar(t, `SELECT host_id FROM volumes WHERE id='vol-1'`); got != "host-b" {
		t.Errorf("host_id = %q, want host-b; the claim did not land", got)
	}
}
