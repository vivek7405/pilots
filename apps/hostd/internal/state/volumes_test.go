package state

import (
	"context"
	"errors"
	"testing"
)

func TestVolumeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	want := &Volume{
		ID: "vol-1", Name: "pgdata", MachineID: "m-1", SizeMiB: 4096,
		S3Prefix: "volumes/vol-1/", MountPath: "/data", HostID: "host-a",
		CreatedAt: 1700000000,
	}
	if err := s.PutVolume(ctx, want); err != nil {
		t.Fatalf("PutVolume: %v", err)
	}

	got, err := s.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if *got != *want {
		t.Fatalf("round trip changed the row:\n got %+v\nwant %+v", *got, *want)
	}

	// The move a rescue makes: same volume, new owner, same data.
	want.HostID = "host-b"
	if err := s.PutVolume(ctx, want); err != nil {
		t.Fatalf("PutVolume after a move: %v", err)
	}
	got, err = s.GetVolume(ctx, "vol-1")
	if err != nil {
		t.Fatalf("GetVolume after a move: %v", err)
	}
	if got.HostID != "host-b" {
		t.Fatalf("volume still owned by %q after moving", got.HostID)
	}
}

func TestGetVolumeMissing(t *testing.T) {
	if _, err := openTest(t).GetVolume(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unknown volume, got %v", err)
	}
}

func TestListVolumes(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, id := range []string{"vol-b", "vol-a"} {
		if err := s.PutVolume(ctx, &Volume{ID: id, HostID: "host-a", SizeMiB: 1024}); err != nil {
			t.Fatalf("PutVolume %s: %v", id, err)
		}
	}
	got, err := s.ListVolumes(ctx)
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(got) != 2 || got[0].ID != "vol-a" || got[1].ID != "vol-b" {
		t.Fatalf("expected both volumes in id order, got %+v", got)
	}
}
