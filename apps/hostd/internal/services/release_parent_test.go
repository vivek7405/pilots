package services

import (
	"context"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A release must never adopt a checkpoint's disk that was diffed against the
// release's OWN image.
//
// A checkpoint's rootfs build is a diff against the machine's template, and
// createFromRelease attaches the host's golden template to every release
// restore. For a release rolled out from a build those differ: the replica was
// booted from the image, so its template IS the image. Adopting that diff
// points the release at a build whose parent no restore attaches,
// block.SetParent catches the mismatch, and the block server exits with
// "handler exited before the device came online" -- so replica two of the
// deploy never comes up, which is how this was found.
func TestAReleaseKeepsItsImageWhenTheCheckpointWasDiffedAgainstIt(t *testing.T) {
	ctx := context.Background()
	store, host, _ := autoscaleFixture(t)
	m := New(Options{HostID: host, Store: store})

	const image = "image-build-1"
	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-1", Name: "m-1", HostID: host, State: "running",
		// Booted FROM the image, so the image is this machine's template and
		// every checkpoint of it is a diff against the image.
		TemplateRootfsBuildID: image,
	}); err != nil {
		t.Fatal(err)
	}

	rel := &state.Release{ID: "rel-1", ServiceID: "svc-1", RootfsBuildID: image}
	if !m.checkpointSharesTheReleaseImage(ctx, "m-1", rel) {
		t.Fatal("a checkpoint diffed against the release's own image was treated " +
			"as adoptable; replica two would fail to attach its disk")
	}
}

// The other half: a replica that was RESTORED carries the golden template, so
// its checkpoint's diff has the same parent a release restore attaches, and
// the release takes it. This is what a rollback and a resize rely on.
func TestAReleaseAdoptsACheckpointDiffedAgainstTheSharedTemplate(t *testing.T) {
	ctx := context.Background()
	store, host, _ := autoscaleFixture(t)
	m := New(Options{HostID: host, Store: store})

	if err := store.PutMachine(ctx, &state.Machine{
		ID: "m-1", Name: "m-1", HostID: host, State: "running",
		TemplateRootfsBuildID: "golden-build",
	}); err != nil {
		t.Fatal(err)
	}

	rel := &state.Release{ID: "rel-1", ServiceID: "svc-1", RootfsBuildID: "image-build-1"}
	if m.checkpointSharesTheReleaseImage(ctx, "m-1", rel) {
		t.Fatal("a checkpoint diffed against the shared template was refused; " +
			"a rollback would lose the disk it just captured")
	}
}

// A row that cannot be read errs towards keeping the image: the boot's disk
// writes are still in the memory image, while adopting a diff whose parent
// nothing attaches costs every replica after the first.
func TestAnUnreadableRowKeepsTheReleaseOnItsImage(t *testing.T) {
	ctx := context.Background()
	store, host, _ := autoscaleFixture(t)
	m := New(Options{HostID: host, Store: store})

	rel := &state.Release{ID: "rel-1", ServiceID: "svc-1", RootfsBuildID: "image-build-1"}
	if !m.checkpointSharesTheReleaseImage(ctx, "no-such-machine", rel) {
		t.Fatal("an unreadable machine row was treated as adoptable")
	}
}
