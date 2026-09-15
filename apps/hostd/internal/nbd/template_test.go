package nbd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// bucket is an object store in memory, counting range reads so a test can tell
// a local read from a fetch.
type bucket struct {
	objects map[string][]byte
	ranges  int
}

func (b *bucket) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("bucket: no such key %s", key)
	}
	return body, nil
}

func (b *bucket) GetRange(_ context.Context, key string, off, length int64) ([]byte, error) {
	body, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("bucket: no such key %s", key)
	}
	b.ranges++
	if off >= int64(len(body)) {
		return nil, fmt.Errorf("bucket: %w", block.ErrRangeNotSatisfiable)
	}
	return body[off:min(off+length, int64(len(body)))], nil
}

// readAll reads a template back the way the overlay would.
func readAll(t *testing.T, s block.Slicer) []byte {
	t.Helper()
	out := make([]byte, 0, s.Size())
	for off := int64(0); off < s.Size(); {
		chunk, err := s.Slice(context.Background(), off, s.Size()-off)
		if err != nil {
			t.Fatalf("Slice at %d: %v", off, err)
		}
		if len(chunk) == 0 {
			t.Fatalf("Slice at %d returned nothing", off)
		}
		out = append(out, chunk...)
		off += int64(len(chunk))
	}
	return out
}

// The template's truth is the build in object storage and the local directory
// is a cache of it, so which one the handler reads is decided by whether the
// cache is whole -- not by which of the two the caller happened to name. A
// complete directory is read with no object storage in the path; anything
// less is served from the bucket into that same directory; and a directory
// that is incomplete with nothing to fall back to is refused rather than
// served as zeros. This is what lets a rescue on a cold host attach at once.
func TestOpenTemplateReadsACompleteCacheAndServesTheBucketOtherwise(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	in := filepath.Join(dir, "in.bin")
	want := bytes.Repeat([]byte{7}, 4096*3)
	if err := os.WriteFile(in, want, 0o644); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	cacheRoot := filepath.Join(dir, "builds")
	templateDir := filepath.Join(cacheRoot, id.String())
	if _, _, err := block.Chunkify(ctx, block.ChunkifyOpts{
		In: in, OutDir: templateDir, BuildID: id,
	}); err != nil {
		t.Fatal(err)
	}
	store := &bucket{objects: map[string][]byte{}}
	for _, name := range []string{"header", "data"} {
		raw, err := os.ReadFile(filepath.Join(templateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		store.objects[id.String()+"/"+name] = raw
	}
	cfg := Config{TemplateDir: templateDir, TemplateBuildID: id, CacheRoot: cacheRoot}

	// Complete on disk: read locally, and it does not matter that there is no
	// object storage at all.
	tpl, closeTpl, err := openTemplate(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("a complete local build was not opened: %v", err)
	}
	if got := readAll(t, tpl); !bytes.Equal(got, want) {
		t.Error("the complete local build read back wrong")
	}
	closeTpl()

	// The same directory with its marker gone is what a killed pull leaves:
	// the right files, the right length, unknown holes. Served from the
	// bucket, into this directory, without a byte of the holes.
	if err := os.Remove(filepath.Join(templateDir, "data.complete")); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(templateDir, "data"), 0); err != nil {
		t.Fatal(err)
	}
	tpl, closeTpl, err = openTemplate(ctx, cfg, store)
	if err != nil {
		t.Fatalf("an incomplete local build was not served from the bucket: %v", err)
	}
	if got := readAll(t, tpl); !bytes.Equal(got, want) {
		t.Error("the bucket-served template read back wrong")
	}
	if store.ranges == 0 {
		t.Error("an incomplete cache was served without touching the bucket")
	}
	closeTpl()
	if !block.BuildComplete(templateDir) {
		t.Error("reading the whole template from the bucket did not complete the cache")
	}

	// Incomplete, and nothing to fall back to: refused, never served.
	if err := os.Remove(filepath.Join(templateDir, "data.complete")); err != nil {
		t.Fatal(err)
	}
	_, _, err = openTemplate(ctx, Config{TemplateDir: templateDir}, store)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("an incomplete build with no build id got %v, want a refusal", err)
	}
	_, _, err = openTemplate(ctx, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "no object storage") {
		t.Fatalf("an incomplete build with no store got %v, want a refusal", err)
	}
}
