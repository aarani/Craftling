package squashfs

import (
	"encoding/binary"
	"errors"
	"io"
)

// idsPerMetaBlock is the maximum number of u32 IDs that fit in one
// 8 KiB metadata block.
const idsPerMetaBlock = int(MetadataBlockSize) / 4

// maxIDs is the format's upper bound on unique UID/GID entries —
// the superblock's id_count field is a u16.
const maxIDs = 0xFFFF

// idTable accumulates the unique UID/GID values referenced by
// inodes and emits them as a squashfs ID table (compressed metadata
// blocks holding u32 IDs, preceded at a separate file offset by a
// lookup list of u64 byte-offsets pointing at each metadata block).
//
// Callers Intern() each UID and GID they encounter (returning a
// u16 index for the inode body), then write the data blocks via
// WriteData and the lookup list via WriteLookup.
type idTable struct {
	index map[uint32]uint16
	list  []uint32
}

func newIDTable() *idTable {
	return &idTable{index: map[uint32]uint16{}}
}

// Intern returns the existing index for id, or assigns a new one.
// Returns an error if the table would exceed the u16 limit.
func (t *idTable) Intern(id uint32) (uint16, error) {
	if idx, ok := t.index[id]; ok {
		return idx, nil
	}
	if len(t.list) >= maxIDs {
		return 0, errors.New("squashfs: more than 65535 distinct UID/GID values")
	}
	idx := uint16(len(t.list))
	t.list = append(t.list, id)
	t.index[id] = idx
	return idx, nil
}

// Count returns the number of unique IDs interned so far. Becomes
// the superblock's id_count field.
func (t *idTable) Count() uint16 { return uint16(len(t.list)) }

// WriteData serializes the ID table's data section: the u32 IDs
// packed into metadata blocks of up to idsPerMetaBlock entries
// each, compressed via comp. The returned slice contains the byte
// offsets (relative to the *file*) of each metadata block — the
// caller supplies fileOffsetBase, the file offset where the first
// metadata block will land, and WriteData accumulates from there.
//
// The lookup list emitted by WriteLookup uses these offsets.
func (t *idTable) WriteData(w io.Writer, comp Compressor, fileOffsetBase uint64) (blockOffsets []uint64, bytesWritten uint64, err error) {
	if len(t.list) == 0 {
		return nil, 0, nil
	}

	meta := newMetaWriter(comp)
	// Track the metadata-block index at which each batch of IDs
	// starts. We bound batches at idsPerMetaBlock IDs so each
	// batch produces exactly one block — which makes the
	// lookup-list (one u64 per block) trivial to construct.
	var startBlocks []int
	buf := make([]byte, 4)
	for i, id := range t.list {
		if i%idsPerMetaBlock == 0 {
			// Seal the previous block before starting a new one
			// so this batch lands in its own metadata block.
			if i != 0 {
				if err := meta.Flush(); err != nil {
					return nil, 0, err
				}
			}
			startBlocks = append(startBlocks, len(meta.blocks))
		}
		binary.LittleEndian.PutUint32(buf, id)
		if _, err := meta.Write(buf); err != nil {
			return nil, 0, err
		}
	}
	if err := meta.Flush(); err != nil {
		return nil, 0, err
	}

	// Convert per-batch block indices to per-block file offsets.
	// We only ever advance one block per batch (because batches are
	// capped at idsPerMetaBlock IDs), so startBlocks has exactly
	// one entry per metadata block.
	for _, bi := range startBlocks {
		blockOffsets = append(blockOffsets, fileOffsetBase+meta.BlockByteOffset(bi))
	}

	bytesWritten, err = meta.EmitTo(w)
	return blockOffsets, bytesWritten, err
}

// WriteLookup writes the ID-table lookup list — a sequence of
// little-endian u64s, one per ID data block, with no metadata
// header (per the format: "stored uncompressed, not preceded by a
// header"). Returns the number of bytes written.
func (t *idTable) WriteLookup(w io.Writer, blockOffsets []uint64) (uint64, error) {
	if len(blockOffsets) == 0 {
		return 0, nil
	}
	buf := make([]byte, 8*len(blockOffsets))
	for i, off := range blockOffsets {
		binary.LittleEndian.PutUint64(buf[i*8:], off)
	}
	n, err := w.Write(buf)
	return uint64(n), err
}
