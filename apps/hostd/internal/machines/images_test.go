package machines

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// writeLocalBuild lays down the files a complete, already-pulled build has on
// disk. Nothing here reads their contents: what is under test is which
// directory a boot is pointed at, not what the block layer serves from it.
func writeLocalBuild(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"header", "data", "data.complete"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A machine booted from a build has its root SERVED from that build's
// directory, the same one every restored machine reads through, and nothing
// is materialised for it: no per-machine rootfs file, no image cache. That is
// what keeps a host's disk at one copy per build however many machines boot
// it -- the 40 GB fill on the rig was one full-size file per build plus one
// per machine, and neither may come back.
func TestABootedMachineServesItsRootFromItsBuild(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newVolumeTestManager(t)
	m.opts.CacheRoot = t.TempDir()

	buildID := uuid.New()
	dir := filepath.Join(m.buildDir(), buildID.String())
	writeLocalBuild(t, dir)

	row := &state.Machine{ID: "m-1", VCPUs: 1, MemMiB: 512}
	backends, err := m.pinBootTemplate(ctx, row, buildID.String())
	if err != nil {
		t.Fatalf("pinBootTemplate: %v", err)
	}

	if backends.RootfsTemplateDir != dir {
		t.Errorf("the root is served from %q, want the build's own directory %q",
			backends.RootfsTemplateDir, dir)
	}
	if backends.CacheRoot != m.buildDir() {
		t.Errorf("the handlers' cache root is %q, want the build directory %q",
			backends.CacheRoot, m.buildDir())
	}
	if row.TemplateRootfsBuildID != buildID.String() || row.ImageRef != buildID.String() {
		t.Errorf("the row pins template=%q image=%q, want both %s",
			row.TemplateRootfsBuildID, row.ImageRef, buildID)
	}
	if row.TemplateMemBuildID != uuid.Nil.String() {
		t.Errorf("a booted machine recorded the memory parent %q, want the nil uuid",
			row.TemplateMemBuildID)
	}

	// Nothing but the build's own directory was written under the cache root:
	// no images/ directory, no full-size ext4 anywhere.
	entries, err := os.ReadDir(m.opts.CacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(m.buildDir()) {
			t.Errorf("a boot left %q behind under the cache root; the root must be served, not materialised", e.Name())
		}
	}
}
