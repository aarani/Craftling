package squashfs

import (
	"encoding/binary"
	"errors"
	"io"
)

// metaRef identifies a location inside a metadata-block stream by
// the byte offset of the containing compressed metadata block
// (measured from the start of the stream, after blocks are
// finalized) and the byte offset within the uncompressed contents
// of that block.
//
// The squashfs on-disk encoding packs this as (block << 16) |
// offset; see metaRef.encode.
type metaRef struct {
	// blockIdx is the index of the metadata block in metaWriter's
	// internal slice. Resolved to a byte offset via the prefix sum
	// of preceding compressed block sizes (plus their 2-byte
	// headers) when the stream is finalized.
	blockIdx int

	// offset is the byte offset within the uncompressed metadata
	// block. Always < MetadataBlockSize (8192).
	offset uint16
}

// metaBlock is one finalized metadata block ready to be written to
// the output. The 2-byte on-disk header is computed when the bytes
// are flushed to disk, not stored here.
type metaBlock struct {
	// data is the bytes that will go on disk after the 2-byte
	// header — compressed if uncompressed is false, raw otherwise.
	data []byte

	// uncompressed records whether the compressor declined to
	// shrink this block (or whether the writer chose to store it
	// raw). Determines the top bit of the on-disk header.
	uncompressed bool

	// uncompressedSize is the size of the original block content
	// before compression. Recorded so that prefix-sum offsets used
	// in metaRef resolution can be computed from the unflushed
	// stream too if needed; not written to disk.
	uncompressedSize uint16
}

// metaWriter buffers an arbitrary byte stream and packs it into
// squashfs metadata blocks of MetadataBlockSize bytes (uncompressed)
// each. The writer compresses every full block through the supplied
// Compressor and flips to "stored raw" when the compressor reports
// ErrIncompressible.
//
// Two-stage usage: callers Write into the stream, optionally calling
// Ref() to capture a logical location before/after a Write; then
// call Flush to seal any partial trailing block; then call
// EmitTo(w, &pos) to serialize all blocks to an io.Writer while
// advancing a running byte position. Resolve metaRefs to absolute
// stream-relative offsets via Resolve.
type metaWriter struct {
	comp   Compressor
	cur    []byte // uncompressed bytes for the current open block
	blocks []metaBlock

	// scratch buffers reused across compress calls so the
	// compressor doesn't allocate a fresh slice per block.
	compressBuf []byte
}

func newMetaWriter(c Compressor) *metaWriter {
	return &metaWriter{
		comp: c,
		cur:  make([]byte, 0, MetadataBlockSize),
	}
}

// Ref returns the location of the next byte that will be written.
// Capture this before a Write to remember where a record starts.
func (m *metaWriter) Ref() metaRef {
	return metaRef{
		blockIdx: len(m.blocks),
		offset:   uint16(len(m.cur)),
	}
}

// Write appends p to the metadata stream, sealing full
// MetadataBlockSize blocks as they fill. Writing across a block
// boundary is allowed — the caller's payload is split.
func (m *metaWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		free := int(MetadataBlockSize) - len(m.cur)
		n := len(p)
		if n > free {
			n = free
		}
		m.cur = append(m.cur, p[:n]...)
		written += n
		p = p[n:]
		if len(m.cur) == int(MetadataBlockSize) {
			if err := m.sealCurrent(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// sealCurrent compresses the current open block (if non-empty) and
// appends it to m.blocks, then resets m.cur for the next block.
// A no-op when m.cur is empty.
func (m *metaWriter) sealCurrent() error {
	if len(m.cur) == 0 {
		return nil
	}
	origSize := uint16(len(m.cur))
	m.compressBuf = m.compressBuf[:0]
	out, err := m.comp.Compress(m.compressBuf, m.cur)
	if err != nil && !errors.Is(err, ErrIncompressible) {
		return err
	}

	var block metaBlock
	if errors.Is(err, ErrIncompressible) || len(out) >= len(m.cur) {
		// Store raw — either the compressor refused or the result
		// would not save space.
		raw := make([]byte, len(m.cur))
		copy(raw, m.cur)
		block = metaBlock{data: raw, uncompressed: true, uncompressedSize: origSize}
	} else {
		dst := make([]byte, len(out))
		copy(dst, out)
		block = metaBlock{data: dst, uncompressed: false, uncompressedSize: origSize}
	}
	m.blocks = append(m.blocks, block)
	m.compressBuf = out[:0]
	m.cur = m.cur[:0]
	return nil
}

// Flush seals any in-progress block. Idempotent: after Flush, the
// stream is closed and further Writes will start a new block. Most
// callers Flush exactly once, just before EmitTo.
func (m *metaWriter) Flush() error {
	return m.sealCurrent()
}

// Resolve turns a metaRef into the byte offset of the referenced
// data relative to the start of the (finalized) metadata-block
// stream. The metadata-block stream consists of a sequence of
// (u16 header)(payload) pairs, so block k's byte offset is the sum
// of (2 + len(block.data)) over blocks 0..k-1, then plus the
// in-block offset for the referenced bytes.
//
// For the high-level squashfs "metadata reference" encoding
// (block << 16 | offset), see metaRef.encode — that uses the block
// offset only, not the in-block offset summed in.
func (m *metaWriter) Resolve(r metaRef) uint64 {
	var off uint64
	for i := 0; i < r.blockIdx; i++ {
		off += 2 + uint64(len(m.blocks[i].data))
	}
	return off + uint64(r.offset)
}

// BlockByteOffset returns the byte offset of metadata block k from
// the start of the stream — the value used as the "block" half of a
// squashfs metadata reference.
func (m *metaWriter) BlockByteOffset(blockIdx int) uint64 {
	var off uint64
	for i := 0; i < blockIdx; i++ {
		off += 2 + uint64(len(m.blocks[i].data))
	}
	return off
}

// EmitTo writes the finalized metadata-block stream to w. Each
// block is prefixed with its u16 header (bit 15 set when the block
// is stored uncompressed; lower 15 bits hold the on-disk size).
// EmitTo returns the total number of bytes written and any error
// encountered.
func (m *metaWriter) EmitTo(w io.Writer) (uint64, error) {
	var total uint64
	var hdr [2]byte
	for _, b := range m.blocks {
		if uint32(len(b.data)) > MetadataBlockSize {
			return total, errors.New("squashfs: metadata block exceeds 8KiB on-disk size")
		}
		size := uint16(len(b.data))
		h := size
		if b.uncompressed {
			h |= MetadataUncompressedBit
		}
		binary.LittleEndian.PutUint16(hdr[:], h)
		n, err := w.Write(hdr[:])
		total += uint64(n)
		if err != nil {
			return total, err
		}
		n, err = w.Write(b.data)
		total += uint64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// encode packs the metadata reference into the squashfs on-disk
// u64 form: top 48 bits hold the block's byte offset within its
// table; bottom 16 bits hold the in-block offset. blockByteOffset
// is the caller-resolved byte offset of the containing metadata
// block (from metaWriter.BlockByteOffset).
func (r metaRef) encode(blockByteOffset uint64) uint64 {
	return (blockByteOffset << 16) | uint64(r.offset)
}
