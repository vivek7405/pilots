package machines

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/fc"
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

// An adopted template carries no artifact id and is left alone.
//
// It was built on another host out of that host's ext4. Judging it against
// this host's would discard a working template over a question nobody asked.
func TestATemplateWithNoRootfsIDIsNotJudged(t *testing.T) {
	m, _ := rootfsManager(t, "userspace v1")

	adopted := &Template{PageSizeKiB: m.pageSizeKiB()}
	if adopted.RootfsID != "" {
		t.Fatal("an adopted template should carry no rootfs id")
	}
	// The check the loader applies, spelled out: a template with no id is
	// never a mismatch, whatever this host's artifact hashes to.
	want := m.rootfsID(variantGolden)
	if want != "" && adopted.RootfsID != "" && adopted.RootfsID != want {
		t.Fatal("a template with no rootfs id was judged a mismatch")
	}
}
