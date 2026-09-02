package block

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/google/uuid"
)

// A build chunkified from a file and materialised back must be the same bytes.
// A machine booted from a build reads this file as its root filesystem, so a
// single wrong block is a kernel panic with nothing to point at.
func TestMaterializeRoundTripsAChunkifiedImage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Deliberately mixed: real data, a long run of zeros in the middle, and a
	// tail. The zero run is the case the sparse write has to get right.
	original := make([]byte, 3*1024*1024)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(original[:1024*1024])
	rnd.Read(original[2*1024*1024:])

	src := filepath.Join(dir, "image.ext4")
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}

	buildDir := filepath.Join(dir, "build")
	id := uuid.New()
	if _, _, err := Chunkify(ctx, ChunkifyOpts{In: src, OutDir: buildDir, BuildID: id}); err != nil {
		t.Fatalf("Chunkify: %v", err)
	}

	build, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatalf("OpenLocalBuild: %v", err)
	}
	defer build.Close()

	out := filepath.Join(dir, "restored.ext4")
	if err := Materialize(ctx, build, out); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) {
		t.Fatalf("materialised %d bytes, want %d", len(got), len(original))
	}
	if !bytes.Equal(got, original) {
		for i := range got {
			if got[i] != original[i] {
				t.Fatalf("first difference at byte %d: got %#x want %#x", i, got[i], original[i])
			}
		}
	}
}

// The long zero run stays a hole. This file is reflink-copied once per
// machine, and a reflink of an allocated extent is not free the way a reflink
// of a hole is.
func TestMaterializeWritesSparsely(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	original := make([]byte, 4*1024*1024)
	copy(original, []byte("header"))

	src := filepath.Join(dir, "image")
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	buildDir := filepath.Join(dir, "build")
	if _, _, err := Chunkify(ctx, ChunkifyOpts{In: src, OutDir: buildDir, BuildID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	build, err := OpenLocalBuild(buildDir)
	if err != nil {
		t.Fatal(err)
	}
	defer build.Close()

	out := filepath.Join(dir, "restored")
	if err := Materialize(ctx, build, out); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(original)) {
		t.Fatalf("apparent size is %d, want %d", info.Size(), len(original))
	}
	allocated := blocksOf(t, out) * 512
	if allocated > int64(len(original))/2 {
		t.Fatalf("the image allocated %d bytes for %d bytes of mostly zeros; "+
			"it was not written sparsely", allocated, len(original))
	}
}

// A half-written image under the final name is indistinguishable from a
// complete one on the next start, and it boots into a kernel panic rather than
// an error.
func TestMaterializeLeavesNothingBehindOnFailure(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "restored")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Materialize(ctx, &stubSlicer{size: 8 << 20}, out); err == nil {
		t.Fatal("a cancelled materialise reported success")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a partial image was left under the final name")
	}
	if _, err := os.Stat(out + ".tmp"); err == nil {
		t.Fatal("the temporary file was left behind")
	}
}

type stubSlicer struct{ size int64 }

func (s *stubSlicer) Size() int64                    { return s.size }
func (s *stubSlicer) BlockSize() int64               { return 4096 }
func (s *stubSlicer) Close() error                   { return nil }
func (s *stubSlicer) Prefault(context.Context) error { return nil }
func (s *stubSlicer) Slice(_ context.Context, _, length int64) ([]byte, error) {
	return make([]byte, length), nil
}

// blocksOf reports the 512-byte blocks a file actually occupies, which is how
// a hole is told from a run of written zeros.
func blocksOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat blocks on this platform")
	}
	return st.Blocks
}
