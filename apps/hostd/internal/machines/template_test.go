package machines

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// hugePageManager returns a Manager whose host is configured for 2MiB pages
// (or 4KiB), with its cache under a temp dir.
func hugePageManager(t *testing.T, huge bool) *Manager {
	t.Helper()
	return &Manager{opts: Options{
		CacheRoot: t.TempDir(),
		FCConfig:  fc.Config{HugePages: huge},
	}}
}

// writeTemplate lays down a manifest plus the build headers loadTemplate
// insists on, so the only thing a test varies is the page size.
func writeTemplate(t *testing.T, m *Manager, tpl *Template) {
	t.Helper()
	if err := os.MkdirAll(m.templateRoot(variantGolden), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{m.memParentDir(tpl), m.rootfsTemplateDir(tpl)} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "header"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.templateRoot(variantGolden), templateFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPageSizeKiBFollowsTheHostSetting(t *testing.T) {
	if got := hugePageManager(t, false).pageSizeKiB(); got != 4 {
		t.Errorf("4KiB host reports %d KiB", got)
	}
	if got := hugePageManager(t, true).pageSizeKiB(); got != 2048 {
		t.Errorf("2MiB host reports %d KiB", got)
	}
}

// A template photographed at another page size cannot be restored here at
// all: Firecracker reads the size back out of the snapshot and refuses to
// reinterpret it. Reporting it as absent rebuilds; believing it would fail
// every create on this host at restore time instead, naming neither the page
// size nor the manifest.
func TestLoadTemplateRejectsAForeignPageSize(t *testing.T) {
	m := hugePageManager(t, true) // this host runs 2MiB
	writeTemplate(t, m, &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		PageSizeKiB:   4, // photographed at 4KiB
	})

	_, err := m.loadTemplate(variantGolden)
	if err == nil {
		t.Fatal("a 4KiB template was accepted by a 2MiB host")
	}
	if !errors.Is(err, errTemplatePageSize) {
		t.Errorf("error was %v, want it to wrap errTemplatePageSize", err)
	}
}

// The mirror case: a 2MiB image on a host that has gone back to 4KiB.
func TestLoadTemplateRejectsAHugePageTemplateOnASmallPageHost(t *testing.T) {
	m := hugePageManager(t, false)
	writeTemplate(t, m, &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		PageSizeKiB:   2048,
	})

	if _, err := m.loadTemplate(variantGolden); !errors.Is(err, errTemplatePageSize) {
		t.Errorf("error was %v, want it to wrap errTemplatePageSize", err)
	}
}

// A manifest written before page size was recorded carries zero, which is not
// a claim that it is 4KiB -- it is unknown, and unknown cannot be restored
// against safely.
func TestLoadTemplateRejectsAManifestWithNoPageSize(t *testing.T) {
	m := hugePageManager(t, false)
	writeTemplate(t, m, &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		// PageSizeKiB left at zero, as a pre-change manifest would have it.
	})

	if _, err := m.loadTemplate(variantGolden); !errors.Is(err, errTemplatePageSize) {
		t.Errorf("error was %v, want it to wrap errTemplatePageSize", err)
	}
}

func TestLoadTemplateAcceptsAMatchingPageSize(t *testing.T) {
	m := hugePageManager(t, true)
	want := &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		PageSizeKiB:   2048,
	}
	writeTemplate(t, m, want)

	got, err := m.loadTemplate(variantGolden)
	if err != nil {
		t.Fatalf("loadTemplate: %v", err)
	}
	if got.PageSizeKiB != 2048 || got.MemBuildID != want.MemBuildID {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
}

// Firecracker rejects an odd mem_size_mib under 2MiB backing, and its error
// names neither the field nor the reason, so it reads like a bug in hostd.
func TestValidateMemMiBRefusesAnOddSizeUnderHugePages(t *testing.T) {
	m := hugePageManager(t, true)
	if err := m.validateMemMiB(513); err == nil {
		t.Error("513 MiB was accepted on a 2MiB host")
	}
	if err := m.validateMemMiB(512); err != nil {
		t.Errorf("512 MiB was refused: %v", err)
	}
}

func TestValidateMemMiBAllowsAnOddSizeAt4KiB(t *testing.T) {
	m := hugePageManager(t, false)
	if err := m.validateMemMiB(513); err != nil {
		t.Errorf("513 MiB was refused on a 4KiB host: %v", err)
	}
}

// A manifest whose vmstate object has been deleted from the bucket passes
// every local check there is: the build headers are on this disk and the page
// size matches, so loadTemplate serves it as good. Nothing local can know
// otherwise, which is why the create path hands the proven-bad key back and
// both sources have to skip it. Without that, discarding the manifest and
// re-deriving returns the same template and the retry fails identically --
// which is exactly how a rig node came to answer 500 to every create for as
// long as it lived.
func TestARejectedTemplateIsNotServedAgain(t *testing.T) {
	m := hugePageManager(t, true)
	const gone = "template/deleted-from-the-bucket/snap.bin"
	tpl := &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		SnapKey:       gone,
		PageSizeKiB:   2048,
	}
	writeTemplate(t, m, tpl)

	// Nothing rejected: the manifest is good as far as anything local knows.
	got, err := m.loadTemplate(variantGolden)
	if err != nil {
		t.Fatalf("loadTemplate: %v", err)
	}
	if got.rejected("") {
		t.Error("the empty key rejected a template; it must reject nothing")
	}
	if !got.rejected(gone) {
		t.Fatalf("the template naming %q was not rejected by its own key", gone)
	}
	// A different key is somebody else's problem.
	if got.rejected("template/some-other-one/snap.bin") {
		t.Error("a template was rejected by a key that is not its own")
	}
}

// Re-deriving means re-reading, and the manifest is what loadTemplate reads.
// A manifest left in place is believed, so discarding it is the only way to
// say "work it out again".
func TestDiscardTemplateForcesAReDerive(t *testing.T) {
	m := hugePageManager(t, true)
	writeTemplate(t, m, &Template{
		MemBuildID:    uuid.New(),
		RootfsBuildID: uuid.New(),
		SnapKey:       "template/x/snap.bin",
		PageSizeKiB:   2048,
	})
	if _, err := m.loadTemplate(variantGolden); err != nil {
		t.Fatalf("loadTemplate before the discard: %v", err)
	}

	m.discardTemplate(variantGolden)

	if _, err := m.loadTemplate(variantGolden); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadTemplate after the discard = %v, want the manifest to be gone", err)
	}
	// Idempotent: a host that never had one must not log or fail differently.
	m.discardTemplate(variantGolden)
}

// The retry has to be able to tell the template's own missing artifact from
// any other error, or it either retries what will never work or fails to
// retry what would.
func TestTemplateArtifactMissingUnwrapsToTheSentinel(t *testing.T) {
	err := error(&templateArtifactMissing{
		snapKey: "template/x/snap.bin",
		err:     fmt.Errorf("restore: %w", fc.ErrArtifactMissing),
	})
	if !errors.Is(err, fc.ErrArtifactMissing) {
		t.Error("the wrapper hid the artifact-missing sentinel from errors.Is")
	}
	var missing *templateArtifactMissing
	if !errors.As(err, &missing) || missing.snapKey != "template/x/snap.bin" {
		t.Errorf("errors.As did not recover the snap key: %+v", missing)
	}
	// An unrelated failure must not look like one.
	if errors.As(errors.New("boom"), &missing) {
		t.Error("an unrelated error was taken for a missing template artifact")
	}
}

// A builder machine must be created from the BUILDER template, not the golden
// one. Restoring a builder from the golden image produces a guest with no
// BuildKit daemon in it, and the first build against it fails on a refused
// connection to a port nothing is listening on. Nothing else in the system
// would report that as a template problem.
//
// The signal is the name prefix, deliberately the same one the quota loop and
// the idle monitor read, so the three cannot disagree about what a builder is.
func TestABuilderIsCreatedFromTheBuilderTemplate(t *testing.T) {
	cases := []struct {
		name string
		want variant
	}{
		{"builder-org1-hosta", variantBuilder},
		{BuilderName("org_1", "host-a"), variantBuilder},
		{BuilderName("", "host-a"), variantBuilder},
		{"web", variantGolden},
		{"buildbot", variantGolden},
		{"", variantGolden},
	}
	for _, tc := range cases {
		if got := variantFor(&state.Machine{Name: tc.name}); got != tc.want {
			t.Errorf("variantFor(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
