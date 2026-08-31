// Package block is the content-addressed storage engine.
//
// A machine's memory and disk are stored as CHUNKED, deduplicated builds
// rather than whole files. A build is a header (a map of logical offset to
// where the bytes actually live) plus a packed data file holding only the
// blocks that are neither zero nor identical to a parent build's.
//
// That indirection is what makes create and wake fast: a restore fetches the
// blocks the guest actually touches, on demand, instead of downloading a
// whole memory image before it can start.
package block

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/google/uuid"
)

// Wire format, little-endian, no padding:
//
//	Header
//	├── Metadata (64 bytes)
//	│   ├── Version     uint64
//	│   ├── BlockSize   uint64
//	│   ├── Size        uint64
//	│   ├── Generation  uint64
//	│   ├── BuildId     [16]byte (UUID)
//	│   └── BaseBuildId [16]byte (UUID)
//	└── Mapping []BuildMap (N × 40 bytes)
//	    ├── Offset             uint64
//	    ├── Length             uint64
//	    ├── BuildId            [16]byte (UUID)
//	    └── BuildStorageOffset uint64
//
// The struct field order IS the wire format: these are serialized with
// binary.Write over the struct directly, so reordering a field silently
// breaks every build already in object storage.

// MetadataVersion is the only version read or written. There is deliberately
// no soft-fail path for an older one -- a build we cannot interpret exactly
// must not be interpreted approximately.
const MetadataVersion uint64 = 3

// MetadataSize is 4×uint64 + 2×16-byte UUID.
const MetadataSize = 64

// BuildMapSize is 2×uint64 + 16-byte UUID + uint64. Note the UUID sits
// between Length and BuildStorageOffset rather than trailing.
const BuildMapSize = 40

// Metadata is the fixed-size header prefix.
type Metadata struct {
	Version     uint64
	BlockSize   uint64
	Size        uint64
	Generation  uint64
	BuildId     uuid.UUID
	BaseBuildId uuid.UUID
}

// BuildMap points one logical range at the bytes that back it.
//
// BuildId names which build's data file holds them: our own for bytes we
// store, or the parent's for a range that was identical and therefore not
// copied.
type BuildMap struct {
	Offset             uint64
	Length             uint64
	BuildId            uuid.UUID
	BuildStorageOffset uint64
}

// Header is a build's metadata plus its mapping, sorted by Offset.
type Header struct {
	Metadata *Metadata
	Mapping  []*BuildMap
}

var (
	// ErrUnsupportedVersion reports a build we cannot read.
	ErrUnsupportedVersion = errors.New("block: unsupported metadata version")
	// ErrGrandparentChain reports a mapping pointing outside the two-level
	// template -> diff chain.
	ErrGrandparentChain = errors.New("block: mapping references a grandparent build")
	// ErrUnalignedOffset reports a lookup that is not block-aligned.
	ErrUnalignedOffset = errors.New("block: offset is not block-aligned")
	// ErrOutOfRange reports a lookup past the end of a build.
	ErrOutOfRange = errors.New("block: offset out of range")
)

// NewTemplateMetadata builds the metadata for a fresh, self-contained build.
func NewTemplateMetadata(buildID uuid.UUID, blockSize, size uint64) *Metadata {
	return &Metadata{
		Version:     MetadataVersion,
		Generation:  0,
		BlockSize:   blockSize,
		Size:        size,
		BuildId:     buildID,
		BaseBuildId: buildID,
	}
}

// NextGeneration derives the metadata for a diff taken against this build.
//
// BaseBuildId is inherited unchanged, so it always names the template at the
// root of the chain no matter how the diff was produced.
func (m *Metadata) NextGeneration(buildID uuid.UUID) *Metadata {
	return &Metadata{
		Version:     MetadataVersion,
		Generation:  m.Generation + 1,
		BlockSize:   m.BlockSize,
		Size:        m.Size,
		BuildId:     buildID,
		BaseBuildId: m.BaseBuildId,
	}
}

// NewHeader assembles a header, sorting the mapping and validating the chain.
//
// An empty mapping synthesizes one full-file self-mapping, which is what a
// fully-zero input produces.
func NewHeader(metadata *Metadata, mapping []*BuildMap) (*Header, error) {
	if len(mapping) == 0 {
		mapping = []*BuildMap{{
			Offset:             0,
			Length:             metadata.Size,
			BuildId:            metadata.BuildId,
			BuildStorageOffset: 0,
		}}
	}
	sort.Slice(mapping, func(i, j int) bool { return mapping[i].Offset < mapping[j].Offset })

	h := &Header{Metadata: metadata, Mapping: mapping}
	if err := h.validateChain(); err != nil {
		return nil, err
	}
	return h, nil
}

// validateChain enforces that a build is at most one diff away from its
// template.
//
// Chunkify compares bytes against the parent to decide what to store, and that
// comparison is only meaningful if the parent is self-contained. A grandparent
// reference means the diff was encoded against bytes nobody can now reproduce
// -- so this has to fail at parse time, where it names the problem, rather than
// at fault time, where it looks like corruption.
func (h *Header) validateChain() error {
	if h.Metadata.Generation > 1 {
		return fmt.Errorf("%w: generation %d (only template->diff is supported)",
			ErrGrandparentChain, h.Metadata.Generation)
	}
	for _, m := range h.Mapping {
		if m.BuildId == uuid.Nil {
			continue // a gap
		}
		if m.BuildId != h.Metadata.BuildId && m.BuildId != h.Metadata.BaseBuildId {
			return fmt.Errorf("%w: mapping at offset %d names build %s, "+
				"which is neither this build %s nor its base %s",
				ErrGrandparentChain, m.Offset, m.BuildId, h.Metadata.BuildId, h.Metadata.BaseBuildId)
		}
	}
	return nil
}

// Serialize writes the header in wire format.
func Serialize(metadata *Metadata, mapping []*BuildMap) ([]byte, error) {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, metadata); err != nil {
		return nil, fmt.Errorf("block: write metadata: %w", err)
	}
	for _, m := range mapping {
		if err := binary.Write(&buf, binary.LittleEndian, m); err != nil {
			return nil, fmt.Errorf("block: write mapping: %w", err)
		}
	}
	return buf.Bytes(), nil
}

// Deserialize reads a header, rejecting an unknown version or a broken chain.
func Deserialize(r io.Reader) (*Header, error) {
	var metadata Metadata
	if err := binary.Read(r, binary.LittleEndian, &metadata); err != nil {
		return nil, fmt.Errorf("block: read metadata: %w", err)
	}
	if metadata.Version != MetadataVersion {
		return nil, fmt.Errorf("%w: %d (want %d)",
			ErrUnsupportedVersion, metadata.Version, MetadataVersion)
	}

	var mapping []*BuildMap
	for {
		var m BuildMap
		err := binary.Read(r, binary.LittleEndian, &m)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("block: read mapping: %w", err)
		}
		mapping = append(mapping, &m)
	}

	h := &Header{Metadata: &metadata, Mapping: mapping}
	sort.Slice(h.Mapping, func(i, j int) bool { return h.Mapping[i].Offset < h.Mapping[j].Offset })
	if err := h.validateChain(); err != nil {
		return nil, err
	}
	return h, nil
}

// GetShiftedMapping resolves a logical offset to where its bytes live.
//
// It returns the storage offset, how many bytes are contiguously available
// from there, and which build holds them. A NIL build id means a gap: the
// range was all zeros and was never stored, and the length runs to the next
// mapping (or to the end of the build).
func (h *Header) GetShiftedMapping(offset uint64) (storageOffset uint64, length uint64, buildID *uuid.UUID, err error) {
	if offset >= h.Metadata.Size {
		return 0, 0, nil, fmt.Errorf("%w: %d >= %d", ErrOutOfRange, offset, h.Metadata.Size)
	}
	if h.Metadata.BlockSize > 0 && offset%h.Metadata.BlockSize != 0 {
		return 0, 0, nil, fmt.Errorf("%w: %d is not a multiple of %d",
			ErrUnalignedOffset, offset, h.Metadata.BlockSize)
	}

	// First mapping starting strictly after the target; the containing one, if
	// any, is therefore the one before it.
	i := sort.Search(len(h.Mapping), func(i int) bool { return h.Mapping[i].Offset > offset })

	if i == 0 {
		// Before the first mapping: a gap running up to it.
		gapEnd := h.Metadata.Size
		if len(h.Mapping) > 0 {
			gapEnd = h.Mapping[0].Offset
		}
		return 0, gapEnd - offset, nil, nil
	}

	m := h.Mapping[i-1]
	if offset < m.Offset+m.Length {
		shift := offset - m.Offset
		id := m.BuildId
		return m.BuildStorageOffset + shift, m.Length - shift, &id, nil
	}

	// Past the end of the previous mapping: a gap up to the next one.
	gapEnd := h.Metadata.Size
	if i < len(h.Mapping) {
		gapEnd = h.Mapping[i].Offset
	}
	return 0, gapEnd - offset, nil, nil
}
