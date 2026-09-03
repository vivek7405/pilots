package machines

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/fc"
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
	if err := os.MkdirAll(m.templateRoot(), 0o755); err != nil {
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
	if err := os.WriteFile(filepath.Join(m.templateRoot(), templateFile), raw, 0o644); err != nil {
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

	_, err := m.loadTemplate()
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

	if _, err := m.loadTemplate(); !errors.Is(err, errTemplatePageSize) {
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

	if _, err := m.loadTemplate(); !errors.Is(err, errTemplatePageSize) {
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

	got, err := m.loadTemplate()
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
