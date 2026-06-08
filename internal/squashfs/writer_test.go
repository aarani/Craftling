package squashfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bufSeeker adapts a bytes.Buffer + cursor so tests can hand a
// WriteSeeker to the Writer without touching the filesystem.
type bufSeeker struct {
	buf []byte
	pos int64
}

func (b *bufSeeker) Write(p []byte) (int, error) {
	end := b.pos + int64(len(p))
	if int64(cap(b.buf)) < end {
		grown := make([]byte, end, end*2)
		copy(grown, b.buf)
		b.buf = grown
	}
	if int64(len(b.buf)) < end {
		b.buf = b.buf[:end]
	}
	copy(b.buf[b.pos:], p)
	b.pos = end
	return len(p), nil
}

func (b *bufSeeker) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = b.pos + offset
	case io.SeekEnd:
		np = int64(len(b.buf)) + offset
	default:
		return 0, errors.New("bufSeeker: bad whence")
	}
	if np < 0 {
		return 0, errors.New("bufSeeker: negative position")
	}
	b.pos = np
	return np, nil
}

// TestSuperblockShape exercises the simplest possible image — root
// directory with a few children — and checks the superblock's
// invariants: magic, version, block size, table-offset ordering,
// bytes_used matching the buffer length.
func TestSuperblockShape(t *testing.T) {
	out := &bufSeeker{}
	w, err := NewWriter(out, WithCompressor(StoredCompressor{}))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	ts := time.Unix(1_700_000_000, 0)
	attrs := func(p string, mode uint32) Attrs {
		return Attrs{Path: p, Mode: 0, UID: 0, GID: 0, Mtime: ts}
	}
	if err := w.CreateDir(Attrs{Path: "/etc", Mode: 0o755, Mtime: ts}); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	fw, err := w.CreateFile(attrs("/etc/hostname", 0o644))
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if _, err := io.WriteString(fw, "hpcc\n"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.CreateSymlink(attrs("/etc/host", 0o777), "hostname"); err != nil {
		t.Fatalf("CreateSymlink: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(out.buf) < superblockSize {
		t.Fatalf("output too small: %d bytes", len(out.buf))
	}
	sb := out.buf[:superblockSize]
	if got := binary.LittleEndian.Uint32(sb[0:]); got != Magic {
		t.Errorf("magic = %#x, want %#x", got, Magic)
	}
	if got := binary.LittleEndian.Uint16(sb[28:]); got != VersionMajor {
		t.Errorf("version major = %d, want %d", got, VersionMajor)
	}
	if got := binary.LittleEndian.Uint16(sb[30:]); got != VersionMinor {
		t.Errorf("version minor = %d, want %d", got, VersionMinor)
	}
	if got := binary.LittleEndian.Uint32(sb[12:]); got != DefaultBlockSize {
		t.Errorf("block size = %d, want %d", got, DefaultBlockSize)
	}
	if got := binary.LittleEndian.Uint16(sb[22:]); got != blockLogFor(DefaultBlockSize) {
		t.Errorf("block log = %d, want %d", got, blockLogFor(DefaultBlockSize))
	}

	// bytes_used is the unpadded logical size of the squashfs
	// data; the file is padded to a 4 KiB boundary so the kernel's
	// sb_bread can read the last block without short-reading
	// (mksquashfs does the same).
	bytesUsed := binary.LittleEndian.Uint64(sb[40:])
	if bytesUsed > uint64(len(out.buf)) {
		t.Errorf("bytes_used = %d exceeds output len = %d", bytesUsed, len(out.buf))
	}
	if uint64(len(out.buf))%4096 != 0 {
		t.Errorf("output len = %d, want 4KiB-aligned", len(out.buf))
	}
	if pad := uint64(len(out.buf)) - bytesUsed; pad >= 4096 {
		t.Errorf("padding = %d, want < 4096 (any larger and bytes_used should have advanced)", pad)
	}

	inodeStart := binary.LittleEndian.Uint64(sb[64:])
	dirStart := binary.LittleEndian.Uint64(sb[72:])
	idTableStart := binary.LittleEndian.Uint64(sb[48:])
	if !(superblockSize <= inodeStart && inodeStart <= dirStart && dirStart <= idTableStart && idTableStart <= bytesUsed) {
		t.Errorf("table-offset ordering violated: super=%d inode=%d dir=%d id=%d used=%d",
			superblockSize, inodeStart, dirStart, idTableStart, bytesUsed)
	}

	// Skipped tables must carry the all-ones sentinel.
	for off, name := range map[int]string{56: "xattr", 80: "frag", 88: "export"} {
		if got := binary.LittleEndian.Uint64(sb[off:]); got != 0xFFFFFFFFFFFFFFFF {
			t.Errorf("%s table start = %#x, want sentinel", name, got)
		}
	}

	// Compressor ID must be a known squashfs constant (we used Stored,
	// which reports as gzip).
	if got := binary.LittleEndian.Uint16(sb[20:]); got != uint16(CompressorGzip) {
		t.Errorf("compressor = %d, want %d", got, CompressorGzip)
	}

	// inode_count is the number of distinct inodes. We created
	// 3 entries (/etc, /etc/hostname, /etc/host) plus the
	// synthesized root → 4. Hardlinks would not count; we used
	// none here.
	if got := binary.LittleEndian.Uint32(sb[4:]); got != 4 {
		t.Errorf("inode_count = %d, want 4", got)
	}
}

// TestRejectsEmptyImage confirms a writer with no entries still
// closes cleanly — the synthesized root directory is enough.
func TestEmptyImageHasRoot(t *testing.T) {
	out := &bufSeeker{}
	w, err := NewWriter(out, WithCompressor(StoredCompressor{}))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := binary.LittleEndian.Uint32(out.buf[4:]); got != 1 {
		t.Errorf("inode_count = %d, want 1 (just root)", got)
	}
}

// TestPathValidation rejects malformed paths.
func TestPathValidation(t *testing.T) {
	bad := []string{"", "/", "etc", "/etc/", "/./x", "/x/../y", "//x"}
	for _, p := range bad {
		out := &bufSeeker{}
		w, _ := NewWriter(out, WithCompressor(StoredCompressor{}))
		if err := w.CreateDir(Attrs{Path: p, Mode: 0o755}); err == nil {
			t.Errorf("CreateDir(%q): expected error", p)
		}
	}
}

// TestHardlinkRequiresPriorFile rejects a hardlink whose target
// has not been created.
func TestHardlinkRequiresPriorFile(t *testing.T) {
	out := &bufSeeker{}
	w, _ := NewWriter(out, WithCompressor(StoredCompressor{}))
	if err := w.CreateHardlink("/a", "/b"); !errors.Is(err, ErrHardlinkTarget) {
		t.Fatalf("CreateHardlink without target: %v, want ErrHardlinkTarget", err)
	}
}

// TestStaleFileWriter checks that the io.Writer returned from a
// previous CreateFile rejects further writes after another Create*.
func TestStaleFileWriter(t *testing.T) {
	out := &bufSeeker{}
	w, _ := NewWriter(out, WithCompressor(StoredCompressor{}))
	fw1, err := w.CreateFile(Attrs{Path: "/a", Mode: 0o644})
	if err != nil {
		t.Fatalf("CreateFile a: %v", err)
	}
	if _, err := fw1.Write([]byte("x")); err != nil {
		t.Fatalf("Write a: %v", err)
	}
	if _, err := w.CreateFile(Attrs{Path: "/b", Mode: 0o644}); err != nil {
		t.Fatalf("CreateFile b: %v", err)
	}
	if _, err := fw1.Write([]byte("y")); !errors.Is(err, ErrStaleEntry) {
		t.Errorf("stale write: %v, want ErrStaleEntry", err)
	}
}

// TestWriteToDisk creates a small image on a real temp file so the
// test harness can produce something a developer can manually feed
// to `unsquashfs -ll` for sanity. The test only verifies the magic
// number; mount-level validation requires Linux + root.
func TestWriteToDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.sqsh")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := NewWriter(f, WithCompressor(StoredCompressor{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.CreateDir(Attrs{Path: "/dir", Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFile(Attrs{Path: "/dir/hello", Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(fw, strings.Repeat("A", 300_000)) // exercise multi-block path
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:4], []byte{0x68, 0x73, 0x71, 0x73}) {
		t.Errorf("magic bytes mismatch: %x", got[:4])
	}
	t.Logf("wrote %d-byte image at %s", len(got), p)
}
