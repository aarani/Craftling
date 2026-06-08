package squashfs

import (
	"bytes"
	"compress/zlib"
	"fmt"
)

// Compressor is the pluggable compression backend for data blocks
// and metadata blocks. The package does not import any compression
// library — the caller picks the dependency (e.g.
// github.com/klauspost/compress/zstd) and supplies a Compressor
// implementation.
//
// One Compressor is shared across all blocks in an image; squashfs
// stores a single CompressorID in the superblock, so mixing
// algorithms within one image is not allowed.
//
// Implementations must be safe to call concurrently — the writer may
// compress multiple blocks in parallel once the implementation lands.
type Compressor interface {
	// ID returns the squashfs compressor ID for this algorithm. It
	// is written into the superblock and must be stable across a
	// single Writer's lifetime.
	ID() CompressorID

	// Options returns the compressor options block embedded after
	// the superblock when the image sets FlagCompressorOptions.
	// Return nil to omit the options block entirely (most callers).
	// The exact layout is compressor-specific; see the format spec.
	Options() []byte

	// Compress appends the compressed form of src to dst and
	// returns the extended slice. If the compressed output would
	// not be smaller than src, implementations should return
	// (dst, ErrIncompressible) and the writer will store src raw
	// with the uncompressed bit set.
	//
	// Implementations must not retain src or dst after Compress
	// returns; the writer reuses both buffers.
	Compress(dst, src []byte) ([]byte, error)
}

// StoredCompressor is a no-op compressor that asks the writer to
// store every block raw. It declares itself as gzip in the
// superblock — the kernel never invokes the gzip decoder for blocks
// whose on-disk header bit marks them uncompressed, so the
// resulting image is valid and mountable without pulling a gzip
// dependency into this package.
//
// Useful for tests and for callers who explicitly want an
// uncompressed image (deterministic byte-for-byte output, no
// compression CPU cost). For production rootfs caches a real
// compressor — zstd is the typical choice — produces 2–4× smaller
// images.
type StoredCompressor struct{}

// ID reports gzip. See type doc for why.
func (StoredCompressor) ID() CompressorID { return CompressorGzip }

// Options returns nil; gzip needs no options block, and even if it
// did the writer never produces a gzip-encoded payload here.
func (StoredCompressor) Options() []byte { return nil }

// Compress always returns ErrIncompressible so the writer stores
// every block raw.
func (StoredCompressor) Compress(dst, src []byte) ([]byte, error) {
	return dst, ErrIncompressible
}

// GzipCompressor compresses blocks with the squashfs "gzip"
// algorithm. Despite the name, squashfs's compressor ID 1 expects
// payloads in the zlib stream format (RFC 1950 — a 2-byte header
// plus a deflate stream), NOT the gzip stream format (RFC 1952,
// which has an extra header with magic/filename/mtime fields). This
// type uses compress/zlib from the stdlib, which produces the right
// shape. compress/gzip would produce something the kernel's
// squashfs driver rejects on the first decompression attempt.
//
// Level matches compress/zlib: 1 (fastest) through 9 (best), or 0
// to take the stdlib default (~6). If the compressed output is not
// strictly smaller than the input, Compress returns
// ErrIncompressible so the writer stores the block raw with the
// per-block uncompressed bit set.
type GzipCompressor struct {
	// Level is the compress/zlib compression level. Zero means
	// "stdlib default" (currently 6).
	Level int
}

// ID reports gzip.
func (GzipCompressor) ID() CompressorID { return CompressorGzip }

// Options omits the optional gzip-options block. The format allows
// callers to advertise non-default compression-level, window-bits,
// and strategy via that block, but compress/zlib hardcodes
// window-bits=15 and a single strategy, so there's nothing useful
// to negotiate with the kernel decoder.
func (GzipCompressor) Options() []byte { return nil }

// Compress runs a zlib stream over src; if the compressed result is
// not smaller than the input, returns ErrIncompressible so the
// writer stores src raw. dst's underlying capacity is reused when
// large enough; otherwise the returned slice is freshly allocated.
func (c GzipCompressor) Compress(dst, src []byte) ([]byte, error) {
	level := c.Level
	if level == 0 {
		level = zlib.DefaultCompression
	}
	buf := bytes.NewBuffer(dst[:0])
	zw, err := zlib.NewWriterLevel(buf, level)
	if err != nil {
		return dst, fmt.Errorf("squashfs: zlib writer: %w", err)
	}
	if _, err := zw.Write(src); err != nil {
		return dst, fmt.Errorf("squashfs: zlib compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return dst, fmt.Errorf("squashfs: zlib close: %w", err)
	}
	out := buf.Bytes()
	if len(out) >= len(src) {
		return dst, ErrIncompressible
	}
	return out, nil
}
