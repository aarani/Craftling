package squashfs

import (
	"encoding/binary"
	"errors"
	"sort"
)

// maxDirEntriesPerHeader is the format's cap on entries that may
// follow a single directory header. The `count` field is u32 but
// stored as count-1, and the kernel rejects values above 255 (256
// entries) for the basic-dir code path.
const maxDirEntriesPerHeader = 256

// dirEntry is a sortable view of one child inside a directory: the
// child's name, its inode-table position (containing-block byte
// offset within the inode table, and in-block offset), inode number,
// and basic-type tag. The kind drives the type field of the on-disk
// entry — extended-inode kinds are not produced by v1, so basic
// tags suffice.
type dirEntry struct {
	name           string
	childInode     uint32
	inodeBlockByte uint32 // byte offset of containing metadata block within the inode table
	inodeBlockOff  uint16 // offset within the uncompressed metadata block
	basicType      uint16
}

// writeDirListing serializes the directory listing for one
// directory into dst, batching entries under as few directory
// headers as the format constraints allow. Returns the in-block
// start offset of the listing (taken from the metaWriter's Ref
// before writing) and the total bytes written by this listing into
// the metadata stream. The caller has already arranged for the
// listing to start at a fresh metadata block when alignment is
// needed.
//
// Constraints that force a new directory header (per the format):
//
//   - The target inode-table metadata-block byte offset (start_block)
//     differs from the running header's value.
//   - The signed delta from the header's reference inode number to
//     the next entry's inode number would exceed the i16 range.
//   - The header has already accumulated maxDirEntriesPerHeader
//     entries (count field is stored count-1 and capped at 255).
func writeDirListing(meta *metaWriter, entries []dirEntry) (startRef metaRef, listingBytes uint32, err error) {
	// Sort by name lexicographically — the kernel relies on
	// strcmp ordering for binary searches inside large directories
	// and refuses out-of-order listings.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	startRef = meta.Ref()

	var hdrBuf [12]byte
	entBuf := make([]byte, 0, 8+255)

	// Walk entries grouping them into headers. We materialize each
	// header into the metadata stream once we know its entry count
	// — header records count as (entry_count - 1).
	i := 0
	for i < len(entries) {
		// Probe forward to find the largest run that satisfies
		// all three constraints relative to entries[i].
		base := entries[i]
		groupEnd := i + 1
		for groupEnd < len(entries) && groupEnd-i < maxDirEntriesPerHeader {
			n := entries[groupEnd]
			if n.inodeBlockByte != base.inodeBlockByte {
				break
			}
			delta := int64(n.childInode) - int64(base.childInode)
			if delta > 32767 || delta < -32768 {
				break
			}
			groupEnd++
		}

		count := groupEnd - i
		binary.LittleEndian.PutUint32(hdrBuf[0:], uint32(count-1))
		binary.LittleEndian.PutUint32(hdrBuf[4:], base.inodeBlockByte)
		binary.LittleEndian.PutUint32(hdrBuf[8:], base.childInode)
		if _, err := meta.Write(hdrBuf[:]); err != nil {
			return metaRef{}, 0, err
		}
		listingBytes += 12

		for j := i; j < groupEnd; j++ {
			e := entries[j]
			name := []byte(e.name)
			if len(name) == 0 || len(name) > 256 {
				return metaRef{}, 0, errors.New("squashfs: directory entry name must be 1..256 bytes")
			}
			entBuf = entBuf[:0]
			entBuf = binary.LittleEndian.AppendUint16(entBuf, e.inodeBlockOff)
			delta := int16(int64(e.childInode) - int64(base.childInode))
			entBuf = binary.LittleEndian.AppendUint16(entBuf, uint16(delta))
			entBuf = binary.LittleEndian.AppendUint16(entBuf, e.basicType)
			entBuf = binary.LittleEndian.AppendUint16(entBuf, uint16(len(name)-1))
			entBuf = append(entBuf, name...)
			if _, err := meta.Write(entBuf); err != nil {
				return metaRef{}, 0, err
			}
			listingBytes += uint32(len(entBuf))
		}

		i = groupEnd
	}

	return startRef, listingBytes, nil
}

// basicTypeFor maps an entry's kind to the basic-inode type tag the
// kernel expects in a directory entry. Hardlinks take the target's
// kind because that's the inode they point at.
func basicTypeFor(e *entry) uint16 {
	k := e.kind
	if k == kindHardlink && e.hardlinkTarget != nil {
		k = e.hardlinkTarget.kind
	}
	switch k {
	case kindFile:
		return InodeBasicFile
	case kindDir:
		return InodeBasicDir
	case kindSymlink:
		return InodeBasicSymlink
	case kindBlockDev:
		return InodeBasicBlock
	case kindCharDev:
		return InodeBasicChar
	case kindFIFO:
		return InodeBasicFIFO
	case kindSocket:
		return InodeBasicSocket
	}
	return 0
}
