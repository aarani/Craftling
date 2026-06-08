package squashfs

// On-wire constants from the squashfs 4.0 format.

// Magic is the squashfs superblock magic ("hsqs" little-endian).
const Magic uint32 = 0x73717368

// Format version. Squashfs 4.0 has been the only on-disk version
// since 2009; we do not implement any other.
const (
	VersionMajor uint16 = 4
	VersionMinor uint16 = 0
)

// Block size bounds from the format. Squashfs stores log2(block_size)
// as a uint16 in the superblock alongside the raw size for
// validation; both must agree. 128 KiB (log 17) is the mksquashfs
// default and a reasonable default here too.
const (
	MinBlockSize     uint32 = 4 * 1024
	MaxBlockSize     uint32 = 1024 * 1024
	DefaultBlockSize uint32 = 128 * 1024
)

// Metadata block size. Inode and directory tables are written as a
// stream of compressed "metadata blocks" of at most 8 KiB each, with
// a 16-bit length prefix (high bit set = stored uncompressed). This
// constant is fixed by the format; do not parameterize.
const MetadataBlockSize uint32 = 8 * 1024

// CompressorID identifies the compression algorithm recorded in the
// superblock. Values come from the format spec; the kernel uses the
// same numbering.
type CompressorID uint16

const (
	CompressorGzip CompressorID = 1
	CompressorLZMA CompressorID = 2 // legacy, kernel dropped support
	CompressorLZO  CompressorID = 3
	CompressorXZ   CompressorID = 4
	CompressorLZ4  CompressorID = 5
	CompressorZstd CompressorID = 6
)

// Superblock feature flags. The writer sets these as appropriate
// based on what the image actually contains; callers do not need to
// touch them.
const (
	FlagUncompressedInodes    uint16 = 1 << 0
	FlagUncompressedData      uint16 = 1 << 1
	FlagCheck                 uint16 = 1 << 2 // unused since 4.0
	FlagUncompressedFragments uint16 = 1 << 3
	FlagNoFragments           uint16 = 1 << 4
	FlagAlwaysFragments       uint16 = 1 << 5
	FlagDuplicates            uint16 = 1 << 6
	FlagExportable            uint16 = 1 << 7
	FlagUncompressedXattrs    uint16 = 1 << 8
	FlagNoXattrs              uint16 = 1 << 9
	FlagCompressorOptions     uint16 = 1 << 10
	FlagUncompressedIDs       uint16 = 1 << 11
)

// Inode type tags. "Basic" variants are used when the entry's
// metadata fits a fixed-size record; "extended" variants carry extra
// fields (sparse hints, xattr indices, etc.) and a 32-bit nlink. The
// v1 writer emits basic variants only, plus extended directories
// when a directory's index would exceed the basic-inode field
// widths.
const (
	InodeBasicDir     uint16 = 1
	InodeBasicFile    uint16 = 2
	InodeBasicSymlink uint16 = 3
	InodeBasicBlock   uint16 = 4
	InodeBasicChar    uint16 = 5
	InodeBasicFIFO    uint16 = 6
	InodeBasicSocket  uint16 = 7
	InodeExtDir       uint16 = 8
	InodeExtFile      uint16 = 9
	InodeExtSymlink   uint16 = 10
	InodeExtBlock     uint16 = 11
	InodeExtChar      uint16 = 12
	InodeExtFIFO      uint16 = 13
	InodeExtSocket    uint16 = 14
)

// DataBlockUncompressedBit is set in a data-block size word to mark
// the block as stored uncompressed. Squashfs uses size==0 to mark a
// sparse hole; an uncompressed block of zero length is encoded as
// the bit alone.
const DataBlockUncompressedBit uint32 = 1 << 24

// MetadataUncompressedBit is the equivalent flag in the 16-bit
// metadata block header.
const MetadataUncompressedBit uint16 = 1 << 15

// InvalidFragment is the value written into a file inode's fragment
// index when the file has no tail-fragment. The v1 writer always
// uses this (we skip fragments entirely).
const InvalidFragment uint32 = 0xFFFFFFFF

// InvalidXattr is the value written into an inode's xattr index when
// the inode has no xattrs. The v1 writer always uses this.
const InvalidXattr uint32 = 0xFFFFFFFF
