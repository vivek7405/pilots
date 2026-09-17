package services

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A release adopts the checkpoint's disk whatever the replica was booted from.
//
// The memory image is captured over that disk -- its page cache, its inode
// tables, the block the agent's token was rewritten into -- so a replica that
// restores the memory onto the pristine image instead reads blocks that are
// not there, and answers 401 to its own token install. A version of this kept
// the release on its image when the replica had been booted from it, because
// createFromRelease attached the golden template to every restore and the
// diff's parent was the image; createFromRelease now attaches the parent the
// build's header names, so the diff is restorable and must be taken.
func TestAReleaseAdoptsTheCheckpointDiskOfABootedReplica(t *testing.T) {
	m, _, store, svc := fixture(t, 1)
	ctx := t.Context()

	const image = "image-build-1"
	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-1", Name: "m-1", HostID: m.opts.HostID, State: "running",
		// Booted FROM the image: the image is this machine's template and
		// its checkpoint's disk is a diff against the image, not the golden
		// template.
		TemplateRootfsBuildID: image,
	}); err != nil {
		t.Fatal(err)
	}

	rel := &state.Release{ID: "rel-1", ServiceID: svc.ID, RootfsBuildID: image}
	if err := m.snapshotRelease(ctx, "m-1", rel); err != nil {
		t.Fatal(err)
	}
	if rel.RootfsBuildID != "rootfs-1" {
		t.Fatalf("the release stayed on %s; want the checkpoint's disk rootfs-1, "+
			"which the memory image was captured over", rel.RootfsBuildID)
	}
	if rel.MemBuildID != "mem-1" {
		t.Fatalf("the release names memory %s, want mem-1", rel.MemBuildID)
	}
}
