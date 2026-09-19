package machines

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/pilotsrun/pilots/hostd/internal/block"
)

// A release restores against what its builds were encoded against, read from
// their headers: the build itself for a self-contained one, else the template
// at the root of the chain. This is what lets replica 2 of an IMAGE deploy
// come up -- its checkpoint has no memory parent and its disk is the image --
// where attaching the golden template made both handlers exit.
func TestBuildBaseReadsTheChainRootFromTheHeader(t *testing.T) {
	m := &Manager{opts: Options{CacheRoot: t.TempDir()}}
	write := func(id uuid.UUID, meta *block.Metadata) {
		t.Helper()
		raw, err := block.Serialize(meta, nil)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(m.buildDir(), id.String())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "header"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	template := uuid.New()
	templateMeta := block.NewTemplateMetadata(template, 4096, 1<<20)
	write(template, templateMeta)
	diff := uuid.New()
	write(diff, templateMeta.NextGeneration(diff))
	image := uuid.New()
	write(image, block.NewTemplateMetadata(image, 4096, 1<<20))

	ctx := context.Background()
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want uuid.UUID
	}{
		{"a diff names its template", diff, template},
		{"a template is its own base", template, template},
		{"an image build is its own base", image, image},
	} {
		got, err := m.buildBase(ctx, tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: base %s, want %s", tc.name, got, tc.want)
		}
	}
}
