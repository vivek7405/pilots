package machines

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// rootfsManager is a manager whose golden artifact is a file the test controls.
func rootfsManager(t *testing.T, body string) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Manager{opts: Options{
		CacheRoot: dir,
		FCConfig:  fc.Config{TemplateRootfs: path},
	}}, path
}

// A template is bound to the artifact it was derived from.
//
// # The bug
//
// The golden rootfs carries the whole guest userspace, agent included, and a
// template is a snapshot of a machine booted from it. Nothing connected the
// two: the manifest names build ids and a page size, none of which change when
// the ext4 underneath is replaced. So shipping a host a new rootfs left the old
// template in place and the host kept minting machines from the previous
// userspace, indefinitely and silently -- the machines work, they simply
// answer the way the last release did.
//
// The rig showed it. Its template was two days older than its rootfs, so the
// `busy` field that the current agent emits was absent from every session
// response, and an assertion failed against a fleet whose code was correct.
func TestANewRootfsInvalidatesTheTemplate(t *testing.T) {
	m, path := rootfsManager(t, "userspace v1")

	first := m.rootfsID(variantGolden)
	if first == "" {
		t.Fatal("a readable rootfs has no id, so nothing can be bound to it")
	}

	// The same bytes: the same id, so a host bootstrap that rewrites the file
	// without changing it does not cost a rebuild.
	if again := m.rootfsID(variantGolden); again != first {
		t.Errorf("the id moved without the content changing: %q then %q", first, again)
	}

	if err := os.WriteFile(path, []byte("userspace v2 with a new agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if second := m.rootfsID(variantGolden); second == first {
		t.Fatal("a new rootfs has the same id as the old one, so a template " +
			"derived from the old one would never be rebuilt and every machine " +
			"would keep the previous guest userspace")
	}
}

// A host that cannot read its artifact keeps the template it has.
//
// Throwing one away is not a way to report a missing file, and a host that
// ADOPTED its template never read a local ext4 to build it -- so an absent
// artifact must not invalidate a template that is perfectly usable.
func TestAnUnreadableRootfsJudgesNothing(t *testing.T) {
	m, path := rootfsManager(t, "userspace v1")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := m.rootfsID(variantGolden); got != "" {
		t.Errorf("rootfsID = %q for a missing artifact; it must decline to "+
			"judge rather than answer with something a check would act on", got)
	}
}

// The fleet row a template is published to is keyed by the artifact.
//
// The row carries no column naming its image, so a single row per vendor pool
// let a rebuilt golden image adopt the template of the image before it, for
// ever: nothing ever built a new one, and neither a new agent nor a fixed
// template capture reached a machine. With the artifact in the id, a new image
// looks up a row that does not exist and builds its own.
func TestTheFleetTemplateRowIsKeyedByTheArtifact(t *testing.T) {
	m, path := rootfsManager(t, "userspace v1")
	m.opts.Vendor = "AuthenticAMD"

	first := m.templateRowID(variantGolden)
	if first == state.GoldenTemplateFor("AuthenticAMD") {
		t.Fatalf("row id %q names no artifact; a new image would adopt this one", first)
	}
	if err := os.WriteFile(path, []byte("userspace v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if second := m.templateRowID(variantGolden); second == first {
		t.Errorf("a new rootfs publishes to the same row %q as the old one", first)
	}
}

// A manifest that names no artifact is not trusted once the host knows its
// own: it was adopted from a row that named no image, or written before
// stamping, and nothing says it matches what this host ships.
func TestAnUnstampedTemplateIsReplacedOnceTheArtifactIsKnown(t *testing.T) {
	m, _ := rootfsManager(t, "userspace v1")
	unstamped := &Template{MemBuildID: uuid.New(), RootfsBuildID: uuid.New(), PageSizeKiB: m.pageSizeKiB()}
	writeTemplate(t, m, unstamped)
	if _, err := m.loadTemplate(variantGolden); !errors.Is(err, errTemplateRootfs) {
		t.Errorf("loadTemplate = %v; an unstamped template was believed", err)
	}

	// And one stamped with this host's artifact is still served.
	stamped := *unstamped
	stamped.RootfsID = m.rootfsID(variantGolden)
	writeTemplate(t, m, &stamped)
	if _, err := m.loadTemplate(variantGolden); err != nil {
		t.Errorf("loadTemplate = %v for a template stamped with this host's artifact", err)
	}
}
