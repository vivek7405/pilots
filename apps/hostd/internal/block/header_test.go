package block

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The struct field order is the wire format, so a golden byte layout is the
// only thing that actually pins it. A reordered field would still round-trip
// through our own code while being unreadable by every build already stored.
func TestMetadataWireLayout(t *testing.T) {
	id := mustUUID(t, "11111111-2222-3333-4444-555555555555")
	base := mustUUID(t, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	raw, err := Serialize(&Metadata{
		Version: 3, BlockSize: 4096, Size: 8192, Generation: 1,
		BuildId: id, BaseBuildId: base,
	}, nil)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if len(raw) != MetadataSize {
		t.Fatalf("metadata is %d bytes, want %d", len(raw), MetadataSize)
	}

	// Four little-endian uint64s, then two raw UUIDs.
	for i, want := range []uint64{3, 4096, 8192, 1} {
		got := binary.LittleEndian.Uint64(raw[i*8 : i*8+8])
		if got != want {
			t.Errorf("field %d = %d, want %d", i, got, want)
		}
	}
	if !bytes.Equal(raw[32:48], id[:]) {
		t.Error("BuildId is not at offset 32")
	}
	if !bytes.Equal(raw[48:64], base[:]) {
		t.Error("BaseBuildId is not at offset 48")
	}
}

// The UUID sits BETWEEN Length and BuildStorageOffset, not trailing. Getting
// that wrong yields a header that parses and resolves to the wrong bytes.
func TestBuildMapWireLayout(t *testing.T) {
	id := mustUUID(t, "11111111-2222-3333-4444-555555555555")

	raw, err := Serialize(
		NewTemplateMetadata(id, 4096, 4096),
		[]*BuildMap{{Offset: 4096, Length: 8192, BuildId: id, BuildStorageOffset: 12288}},
	)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if len(raw) != MetadataSize+BuildMapSize {
		t.Fatalf("header is %d bytes, want %d", len(raw), MetadataSize+BuildMapSize)
	}

	m := raw[MetadataSize:]
	if got := binary.LittleEndian.Uint64(m[0:8]); got != 4096 {
		t.Errorf("Offset = %d", got)
	}
	if got := binary.LittleEndian.Uint64(m[8:16]); got != 8192 {
		t.Errorf("Length = %d", got)
	}
	if !bytes.Equal(m[16:32], id[:]) {
		t.Error("BuildId is not between Length and BuildStorageOffset")
	}
	if got := binary.LittleEndian.Uint64(m[32:40]); got != 12288 {
		t.Errorf("BuildStorageOffset = %d", got)
	}
}

func TestSerializeDeserializeRoundTrip(t *testing.T) {
	id := uuid.New()
	meta := NewTemplateMetadata(id, 4096, 16384)
	mapping := []*BuildMap{
		{Offset: 0, Length: 4096, BuildId: id, BuildStorageOffset: 0},
		{Offset: 8192, Length: 8192, BuildId: id, BuildStorageOffset: 4096},
	}

	raw, err := Serialize(meta, mapping)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	got, err := Deserialize(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	if *got.Metadata != *meta {
		t.Errorf("metadata mismatch:\n got %+v\nwant %+v", *got.Metadata, *meta)
	}
	if len(got.Mapping) != len(mapping) {
		t.Fatalf("got %d mappings, want %d", len(got.Mapping), len(mapping))
	}
	for i := range mapping {
		if *got.Mapping[i] != *mapping[i] {
			t.Errorf("mapping %d:\n got %+v\nwant %+v", i, *got.Mapping[i], *mapping[i])
		}
	}
}

func TestDeserializeRejectsUnknownVersion(t *testing.T) {
	id := uuid.New()
	raw, err := Serialize(&Metadata{Version: 2, BlockSize: 4096, Size: 4096,
		BuildId: id, BaseBuildId: id}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Deserialize(bytes.NewReader(raw)); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("got %v, want ErrUnsupportedVersion", err)
	}
}

// Chunkify's byte comparison is only meaningful if the parent is
// self-contained, so a grandparent reference must fail where it can be named
// rather than surfacing later as unexplained corruption.
func TestGrandparentChainIsRejected(t *testing.T) {
	self, base, stranger := uuid.New(), uuid.New(), uuid.New()
	meta := &Metadata{Version: 3, BlockSize: 4096, Size: 8192, Generation: 1,
		BuildId: self, BaseBuildId: base}

	t.Run("foreign build id in a mapping", func(t *testing.T) {
		_, err := NewHeader(meta, []*BuildMap{
			{Offset: 0, Length: 4096, BuildId: stranger, BuildStorageOffset: 0},
		})
		if !errors.Is(err, ErrGrandparentChain) {
			t.Errorf("got %v, want ErrGrandparentChain", err)
		}
	})

	t.Run("generation beyond one", func(t *testing.T) {
		deep := *meta
		deep.Generation = 2
		if _, err := NewHeader(&deep, []*BuildMap{
			{Offset: 0, Length: 4096, BuildId: self, BuildStorageOffset: 0},
		}); !errors.Is(err, ErrGrandparentChain) {
			t.Errorf("got %v, want ErrGrandparentChain", err)
		}
	})

	t.Run("parent reference is legal", func(t *testing.T) {
		if _, err := NewHeader(meta, []*BuildMap{
			{Offset: 0, Length: 4096, BuildId: base, BuildStorageOffset: 0},
			{Offset: 4096, Length: 4096, BuildId: self, BuildStorageOffset: 0},
		}); err != nil {
			t.Errorf("a legal template->diff chain was rejected: %v", err)
		}
	})

	t.Run("rejected at parse time too", func(t *testing.T) {
		raw, err := Serialize(meta, []*BuildMap{
			{Offset: 0, Length: 4096, BuildId: stranger, BuildStorageOffset: 0},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Deserialize(bytes.NewReader(raw)); !errors.Is(err, ErrGrandparentChain) {
			t.Errorf("got %v, want ErrGrandparentChain", err)
		}
	})
}

func TestNextGenerationInheritsBase(t *testing.T) {
	tmpl := uuid.New()
	meta := NewTemplateMetadata(tmpl, 4096, 8192)
	if meta.Generation != 0 || meta.BuildId != meta.BaseBuildId {
		t.Fatalf("a template must be generation 0 with base == self: %+v", meta)
	}

	diffID := uuid.New()
	diff := meta.NextGeneration(diffID)
	if diff.Generation != 1 {
		t.Errorf("generation = %d, want 1", diff.Generation)
	}
	if diff.BuildId != diffID {
		t.Errorf("BuildId = %s, want %s", diff.BuildId, diffID)
	}
	if diff.BaseBuildId != tmpl {
		t.Errorf("BaseBuildId = %s, want the template %s -- the base must name the "+
			"root of the chain, not the immediate parent", diff.BaseBuildId, tmpl)
	}
	if diff.BlockSize != meta.BlockSize || diff.Size != meta.Size {
		t.Error("a diff must describe the same geometry as its parent")
	}
}

// A gap is a range that was all zeros and therefore never stored. Resolving
// one has to report how far the gap runs, or a reader would fill the wrong
// number of zeros.
func TestGetShiftedMappingGapsAndBoundaries(t *testing.T) {
	id := uuid.New()
	// 16 KiB in 4 KiB blocks, with data only in blocks 1 and 3.
	h, err := NewHeader(NewTemplateMetadata(id, 4096, 16384), []*BuildMap{
		{Offset: 4096, Length: 4096, BuildId: id, BuildStorageOffset: 0},
		{Offset: 12288, Length: 4096, BuildId: id, BuildStorageOffset: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		offset      uint64
		wantStorage uint64
		wantLength  uint64
		wantMapped  bool
	}{
		{"gap before the first mapping", 0, 0, 4096, false},
		{"inside the first mapping", 4096, 0, 4096, true},
		{"gap between mappings", 8192, 0, 4096, false},
		{"inside the last mapping", 12288, 4096, 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, length, buildID, err := h.GetShiftedMapping(tc.offset)
			if err != nil {
				t.Fatalf("GetShiftedMapping(%d): %v", tc.offset, err)
			}
			if (buildID != nil) != tc.wantMapped {
				t.Fatalf("mapped = %v, want %v", buildID != nil, tc.wantMapped)
			}
			if length != tc.wantLength {
				t.Errorf("length = %d, want %d", length, tc.wantLength)
			}
			if tc.wantMapped && storage != tc.wantStorage {
				t.Errorf("storage offset = %d, want %d", storage, tc.wantStorage)
			}
		})
	}
}

func TestGetShiftedMappingRejectsBadOffsets(t *testing.T) {
	id := uuid.New()
	h, err := NewHeader(NewTemplateMetadata(id, 4096, 8192), []*BuildMap{
		{Offset: 0, Length: 8192, BuildId: id, BuildStorageOffset: 0},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := h.GetShiftedMapping(8192); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("offset at Size: got %v, want ErrOutOfRange", err)
	}
	if _, _, _, err := h.GetShiftedMapping(100); !errors.Is(err, ErrUnalignedOffset) {
		t.Errorf("unaligned offset: got %v, want ErrUnalignedOffset", err)
	}
}

// An all-zero build has no mappings at all, and every offset in it resolves to
// a gap. Synthesizing a full self-mapping instead would claim the whole file is
// stored while the data file is empty.
func TestNewHeaderEmptyMappingIsAllZero(t *testing.T) {
	id := uuid.New()
	h, err := NewHeader(NewTemplateMetadata(id, 4096, 8192), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Mapping) != 0 {
		t.Fatalf("got %d mappings, want none", len(h.Mapping))
	}

	storage, length, buildID, err := h.GetShiftedMapping(0)
	if err != nil {
		t.Fatalf("GetShiftedMapping: %v", err)
	}
	if buildID != nil {
		t.Error("offset 0 resolved to stored bytes; an all-zero build stores none")
	}
	if length != 8192 || storage != 0 {
		t.Errorf("gap = (storage %d, length %d), want (0, 8192)", storage, length)
	}
}

func TestNewHeaderSortsMapping(t *testing.T) {
	id := uuid.New()
	h, err := NewHeader(NewTemplateMetadata(id, 4096, 12288), []*BuildMap{
		{Offset: 8192, Length: 4096, BuildId: id, BuildStorageOffset: 4096},
		{Offset: 0, Length: 4096, BuildId: id, BuildStorageOffset: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.Mapping[0].Offset != 0 || h.Mapping[1].Offset != 8192 {
		t.Error("mapping was not sorted; the binary search assumes ordering")
	}
}

// A cut-short mapping record is not the end of the mapping -- it is a header
// that was truncated by an interrupted upload or a partial write. Treating it
// as clean EOF drops every mapping after the cut, and the ranges they described
// then read back as zeros: the same silent corruption validateDataSize refuses
// to allow on the data side.
func TestDeserializeRejectsATruncatedHeader(t *testing.T) {
	meta := NewTemplateMetadata(uuid.New(), 4096, 4*4096)
	mapping := []*BuildMap{
		{Offset: 0, Length: 4096, BuildId: meta.BuildId, BuildStorageOffset: 0},
		{Offset: 4096, Length: 4096, BuildId: meta.BuildId, BuildStorageOffset: 4096},
		{Offset: 8192, Length: 4096, BuildId: meta.BuildId, BuildStorageOffset: 8192},
	}
	raw, err := Serialize(meta, mapping)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	// Cut the last record in half, the way an interrupted write would.
	cut := raw[:len(raw)-BuildMapSize/2]
	if _, err := Deserialize(bytes.NewReader(cut)); err == nil {
		t.Error("a header ending in a half-written mapping was accepted; " +
			"the ranges it dropped would read back as zeros")
	}

	// A whole number of records is still fine: that is just a shorter header.
	whole := raw[:len(raw)-BuildMapSize]
	h, err := Deserialize(bytes.NewReader(whole))
	if err != nil {
		t.Fatalf("a complete but shorter header was rejected: %v", err)
	}
	if len(h.Mapping) != 2 {
		t.Errorf("read %d mappings, want 2", len(h.Mapping))
	}
}
