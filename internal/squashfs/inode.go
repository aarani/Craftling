package squashfs

import (
	"encoding/binary"
	"errors"
)

// commonInodeSize is the fixed size of the 16-byte squashfs common
// inode header that precedes every type-specific body.
const commonInodeSize = 16

// entryKind is the kind of filesystem object an entry represents.
// Internal — the public API surface uses Create* methods to spell
// each kind.
type entryKind uint8

const (
	kindFile entryKind = iota + 1
	kindDir
	kindSymlink
	kindHardlink
	kindCharDev
	kindBlockDev
	kindFIFO
	kindSocket
)

// entry is the internal representation of one squashfs inode (or
// one hardlink pointing at another entry's inode). Populated as
// Create* methods run; finalized — inode numbers, link counts,
// metadata-table positions — at Close.
type entry struct {
	path  string
	kind  entryKind
	attrs Attrs

	// Resolved during finalization.
	inodeNumber uint32  // 1-based; root is always 1
	inodeRef    metaRef // location in the inode-table metaWriter
	nlinks      uint32  // hard link count exposed to the kernel
	uidIdx      uint16  // index into ID table
	gidIdx      uint16

	// File-only state, populated during streaming.
	fileBlocksStart uint64   // absolute byte offset in out
	fileBlockSizes  []uint32 // per-block on-disk sizes (with uncompressed bit)
	fileSize        uint64   // logical file size

	// Symlink-only.
	symlinkTarget string

	// Hardlink-only — points at the entry whose inode this hardlink
	// references. Resolved by name at CreateHardlink time so any
	// post-creation rename (which we don't support anyway) wouldn't
	// silently dangle.
	hardlinkTarget *entry

	// Device-only.
	devMajor uint32
	devMinor uint32

	// Directory-only — populated during tree-build at Close.
	children []*entry

	// Directory listing location, populated during the post-order
	// pass over the tree. Absolute byte offset in the directory-
	// table metaWriter for the metadata block containing this
	// dir's listing start; the in-block offset comes from
	// dirBlockOffset; the total listing size (excluding the +3
	// quirk) comes from dirListingSize.
	dirBlockStart  uint64
	dirBlockOffset uint16
	dirListingSize uint32

	parentInode uint32 // resolved during tree build
}

// encodePerm extracts the Unix permission bits from the entry's
// mode. The type bits of fs.FileMode are deliberately discarded —
// the entry's kind comes from which Create* method was called.
func encodePerm(e *entry) uint16 {
	return uint16(e.attrs.Mode.Perm()) | uint16(e.attrs.Mode&07000)
}

// encodeDevice packs a major/minor pair into the 32-bit "device
// number" field. The format constrains minor to 8 bits and major to
// 12 bits; larger values are rejected to avoid silent truncation.
func encodeDevice(major, minor uint32) (uint32, error) {
	if major > 0xFFF {
		return 0, errors.New("squashfs: device major exceeds 12-bit limit")
	}
	if minor > 0xFF {
		return 0, errors.New("squashfs: device minor exceeds 8-bit limit (squashfs format constraint)")
	}
	return (major << 8) | minor, nil
}

// writeCommonHeader emits the 16-byte common inode header.
func writeCommonHeader(buf []byte, typ uint16, e *entry, mtime uint32) {
	binary.LittleEndian.PutUint16(buf[0:], typ)
	binary.LittleEndian.PutUint16(buf[2:], encodePerm(e))
	binary.LittleEndian.PutUint16(buf[4:], e.uidIdx)
	binary.LittleEndian.PutUint16(buf[6:], e.gidIdx)
	binary.LittleEndian.PutUint32(buf[8:], mtime)
	binary.LittleEndian.PutUint32(buf[12:], e.inodeNumber)
}

// encodeBasicDir serializes a basic_dir inode (type 1).
//
//	common header (16) + body (16) = 32 bytes
//
// Body fields:
//
//	u32 dir_block_start  (byte offset of metadata block inside the directory table)
//	u32 hard_link_count
//	u16 file_size        (listing bytes + 3; the +3 is a format quirk, empty dir => 3)
//	u16 block_offset     (in-metadata-block start of listing)
//	u32 parent_inode
func encodeBasicDir(e *entry, mtime uint32) ([]byte, error) {
	if e.dirBlockStart > 0xFFFFFFFF {
		return nil, errors.New("squashfs: directory table exceeds 4 GiB (need extended dir support)")
	}
	listingSize := e.dirListingSize + 3
	if listingSize > 0xFFFF {
		return nil, errors.New("squashfs: directory listing exceeds 64 KiB (need extended dir support)")
	}
	buf := make([]byte, commonInodeSize+16)
	writeCommonHeader(buf, InodeBasicDir, e, mtime)
	body := buf[commonInodeSize:]
	binary.LittleEndian.PutUint32(body[0:], uint32(e.dirBlockStart))
	binary.LittleEndian.PutUint32(body[4:], e.nlinks)
	binary.LittleEndian.PutUint16(body[8:], uint16(listingSize))
	binary.LittleEndian.PutUint16(body[10:], e.dirBlockOffset)
	binary.LittleEndian.PutUint32(body[12:], e.parentInode)
	return buf, nil
}

// encodeBasicFile serializes a basic_file inode (type 2).
//
//	common header (16) + body (16) + 4*N block sizes
//
// Body fields:
//
//	u32 blocks_start   (absolute byte offset in archive of the first data block)
//	u32 frag_index     (0xFFFFFFFF — no fragments in v1)
//	u32 block_offset   (offset into fragment — always 0 since no fragments)
//	u32 file_size
//
// block sizes are 32-bit per-block on-disk sizes; bit 24 set marks
// "stored uncompressed", and the writer already produced sizes in
// that encoding when it streamed file content.
func encodeBasicFile(e *entry, mtime uint32, blockSize uint32) ([]byte, error) {
	if e.fileBlocksStart > 0xFFFFFFFF {
		return nil, errors.New("squashfs: archive exceeds 4 GiB before file inode (need extended file support)")
	}
	if e.fileSize > 0xFFFFFFFF {
		return nil, errors.New("squashfs: file exceeds 4 GiB (need extended file support)")
	}
	expectedBlocks := (e.fileSize + uint64(blockSize) - 1) / uint64(blockSize)
	if uint64(len(e.fileBlockSizes)) != expectedBlocks {
		return nil, errors.New("squashfs: block-size count does not match file size")
	}
	buf := make([]byte, commonInodeSize+16+4*len(e.fileBlockSizes))
	writeCommonHeader(buf, InodeBasicFile, e, mtime)
	body := buf[commonInodeSize:]
	binary.LittleEndian.PutUint32(body[0:], uint32(e.fileBlocksStart))
	binary.LittleEndian.PutUint32(body[4:], InvalidFragment)
	binary.LittleEndian.PutUint32(body[8:], 0)
	binary.LittleEndian.PutUint32(body[12:], uint32(e.fileSize))
	sizes := body[16:]
	for i, s := range e.fileBlockSizes {
		binary.LittleEndian.PutUint32(sizes[i*4:], s)
	}
	return buf, nil
}

// encodeBasicSymlink serializes a basic_symlink inode (type 3).
//
//	common header (16) + body (8 + target_size)
//
// Body fields:
//
//	u32 hard_link_count
//	u32 target_size
//	u8[target_size] target (not null-terminated)
func encodeBasicSymlink(e *entry, mtime uint32) ([]byte, error) {
	target := []byte(e.symlinkTarget)
	if len(target) > 0xFFFF { // reasonable cap; format allows u32 but kernel rejects huge
		return nil, errors.New("squashfs: symlink target exceeds 65535 bytes")
	}
	buf := make([]byte, commonInodeSize+8+len(target))
	writeCommonHeader(buf, InodeBasicSymlink, e, mtime)
	body := buf[commonInodeSize:]
	binary.LittleEndian.PutUint32(body[0:], e.nlinks)
	binary.LittleEndian.PutUint32(body[4:], uint32(len(target)))
	copy(body[8:], target)
	return buf, nil
}

// encodeBasicDevice serializes a basic_block (type 4) or basic_char
// (type 5) device inode.
//
//	common header (16) + body (8)
//
// Body fields:
//
//	u32 hard_link_count
//	u32 device_number  (major << 8 | minor)
func encodeBasicDevice(e *entry, mtime uint32, block bool) ([]byte, error) {
	dev, err := encodeDevice(e.devMajor, e.devMinor)
	if err != nil {
		return nil, err
	}
	typ := InodeBasicChar
	if block {
		typ = InodeBasicBlock
	}
	buf := make([]byte, commonInodeSize+8)
	writeCommonHeader(buf, typ, e, mtime)
	body := buf[commonInodeSize:]
	binary.LittleEndian.PutUint32(body[0:], e.nlinks)
	binary.LittleEndian.PutUint32(body[4:], dev)
	return buf, nil
}

// encodeBasicIPC serializes a basic_fifo (type 6) or basic_socket
// (type 7) inode.
//
//	common header (16) + body (4)
//
// Body fields:
//
//	u32 hard_link_count
func encodeBasicIPC(e *entry, mtime uint32, socket bool) []byte {
	typ := InodeBasicFIFO
	if socket {
		typ = InodeBasicSocket
	}
	buf := make([]byte, commonInodeSize+4)
	writeCommonHeader(buf, typ, e, mtime)
	binary.LittleEndian.PutUint32(buf[commonInodeSize:], e.nlinks)
	return buf
}
