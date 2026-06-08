package squashfs

import (
	"encoding/binary"
	"math/bits"
)

// superblockSize is the fixed on-disk size of the squashfs 4.0
// superblock. The Writer reserves this many bytes at file offset 0
// at construction time and rewrites it for real at Close.
const superblockSize = 96

// superblock carries the 96-byte squashfs 4.0 superblock fields in
// host form. Marshaled by encode().
type superblock struct {
	inodeCount          uint32
	modTime             uint32
	blockSize           uint32
	fragCount           uint32
	compressor          uint16
	blockLog            uint16
	flags               uint16
	idCount             uint16
	rootInodeRef        uint64
	bytesUsed           uint64
	idTableStart        uint64
	xattrTableStart     uint64
	inodeTableStart     uint64
	directoryTableStart uint64
	fragmentTableStart  uint64
	exportTableStart    uint64
}

// encode serializes the superblock to a 96-byte buffer.
func (sb *superblock) encode() []byte {
	buf := make([]byte, superblockSize)
	binary.LittleEndian.PutUint32(buf[0:], Magic)
	binary.LittleEndian.PutUint32(buf[4:], sb.inodeCount)
	binary.LittleEndian.PutUint32(buf[8:], sb.modTime)
	binary.LittleEndian.PutUint32(buf[12:], sb.blockSize)
	binary.LittleEndian.PutUint32(buf[16:], sb.fragCount)
	binary.LittleEndian.PutUint16(buf[20:], sb.compressor)
	binary.LittleEndian.PutUint16(buf[22:], sb.blockLog)
	binary.LittleEndian.PutUint16(buf[24:], sb.flags)
	binary.LittleEndian.PutUint16(buf[26:], sb.idCount)
	binary.LittleEndian.PutUint16(buf[28:], VersionMajor)
	binary.LittleEndian.PutUint16(buf[30:], VersionMinor)
	binary.LittleEndian.PutUint64(buf[32:], sb.rootInodeRef)
	binary.LittleEndian.PutUint64(buf[40:], sb.bytesUsed)
	binary.LittleEndian.PutUint64(buf[48:], sb.idTableStart)
	binary.LittleEndian.PutUint64(buf[56:], sb.xattrTableStart)
	binary.LittleEndian.PutUint64(buf[64:], sb.inodeTableStart)
	binary.LittleEndian.PutUint64(buf[72:], sb.directoryTableStart)
	binary.LittleEndian.PutUint64(buf[80:], sb.fragmentTableStart)
	binary.LittleEndian.PutUint64(buf[88:], sb.exportTableStart)
	return buf
}

// blockLogFor returns log2(blockSize). The caller has already
// validated that blockSize is a power of two in the configured
// range, so this never produces a fractional or out-of-range value.
func blockLogFor(blockSize uint32) uint16 {
	return uint16(bits.TrailingZeros32(blockSize))
}
