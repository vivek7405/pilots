package block

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// Materializing a build must reproduce every byte that went into it.
//
// The failure this guards is silent and total. A Slicer clamps its answer to
// the mapping an offset lands in, so a read spanning a mapping boundary comes
// back short -- which is most reads on any real image, because zero elision
// cuts a disk into many mappings. Materialize advanced by the size it had
// ASKED for rather than the size it GOT, so everything between the end of a
// short read and the end of the window was never written.
//
// The output still had the right length and a correct first megabyte, so it
// looked plausible. What it actually produced was a built image whose guest
// panicked the kernel at mount_block_root with a corrupt journal inode -- an
// error naming nothing about offsets, mappings, or short reads.
func TestMaterializeReproducesEveryByte(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")

	// The shape that breaks it: content, a long zero run that gets elided into
	// a gap, then more content on the far side of that gap. A file with no
	// gaps has one mapping and round-trips even when the advance is wrong.
	const blocks = 64
	buf := make([]byte, 4096*blocks)
	rand.Read(buf[:4096*8])
	rand.Read(buf[4096*40 : 4096*48])
	copy(buf[4096*63:], []byte("the last block must survive too"))
	if err := os.WriteFile(src, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "build")
	if _, _, err := Chunkify(context.Background(), ChunkifyOpts{
		In: src, OutDir: out, BuildID: uuid.New(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := OpenLocalBuild(out)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	got := filepath.Join(dir, "got.img")
	if err := Materialize(context.Background(), b, got); err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}

	if len(have) < len(buf) {
		t.Fatalf("materialized %d bytes from a %d byte source", len(have), len(buf))
	}
	if !bytes.Equal(buf, have[:len(buf)]) {
		for i := range buf {
			if buf[i] != have[i] {
				t.Fatalf("first difference at byte %d (block %d of %d)",
					i, i/4096, blocks)
			}
		}
	}
	// Chunkify works in whole blocks, so a source that is not a block multiple
	// rounds up. The padding must be zeros, not stale bytes from anywhere else.
	if rest := have[len(buf):]; !allZeroBytes(rest) {
		t.Errorf("%d bytes of non-zero padding past the source", len(rest))
	}
}
