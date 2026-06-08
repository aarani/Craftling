package image

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeRootfsName_roundtrip(t *testing.T) {
	cases := []struct {
		digest string
		file   string
	}{
		{"sha256:abc123", "sha256-abc123.sqsh"},
		{"sha512:deadbeef", "sha512-deadbeef.sqsh"},
		// Bare hex assumed sha256.
		{"abc", "sha256-abc.sqsh"},
	}
	for _, c := range cases {
		got, err := encodeRootfsName(c.digest)
		if err != nil {
			t.Fatalf("encodeRootfsName(%q): %v", c.digest, err)
		}
		if got != c.file {
			t.Errorf("encodeRootfsName(%q) = %q, want %q", c.digest, got, c.file)
		}

		// Decoding always normalizes back to "<algo>:<hex>".
		want := c.digest
		if !strings.Contains(want, ":") {
			want = "sha256:" + want
		}
		dec, ok := decodeRootfsName(c.file)
		if !ok {
			t.Errorf("decodeRootfsName(%q) returned ok=false", c.file)
			continue
		}
		if dec != want {
			t.Errorf("decodeRootfsName(%q) = %q, want %q", c.file, dec, want)
		}
	}
}

func TestEncodeRootfsName_rejectsInvalid(t *testing.T) {
	for _, in := range []string{"", ":abc", "sha256:"} {
		if _, err := encodeRootfsName(in); err == nil {
			t.Errorf("encodeRootfsName(%q) succeeded, want error", in)
		}
	}
}

func TestDecodeRootfsName_rejectsStrays(t *testing.T) {
	for _, name := range []string{
		"random.txt",          // wrong suffix
		"sha256.sqsh",         // no dash
		"-abc.sqsh",           // empty algo
		"sha256-.sqsh",        // empty hex
		".sqsh",               // empty stem
		"sha256-abc.sqsh.bak", // wrong suffix
	} {
		if _, ok := decodeRootfsName(name); ok {
			t.Errorf("decodeRootfsName(%q) returned ok=true, want false", name)
		}
	}
}

func TestGetExistingImages_listsAndFiltersStrays(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sha256-aaaa.sqsh"), nil)
	mustWrite(t, filepath.Join(dir, "sha256-bbbb.sqsh"), nil)
	// Strays the listing must skip silently.
	mustWrite(t, filepath.Join(dir, "in-progress.tmp"), nil)
	mustWrite(t, filepath.Join(dir, "README"), nil)
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := &Store{CacheDir: dir}
	got, err := s.GetExistingImages(context.Background())
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	sort.Strings(got)
	want := []string{"sha256:aaaa", "sha256:bbbb"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGetExistingImages_missingCacheDirIsEmpty(t *testing.T) {
	s := &Store{CacheDir: filepath.Join(t.TempDir(), "does-not-exist")}
	got, err := s.GetExistingImages(context.Background())
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestUntagImage_removesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sha256-cafe.sqsh")
	mustWrite(t, path, []byte("rootfs"))

	s := &Store{CacheDir: dir}
	if err := s.UntagImage(context.Background(), "sha256:cafe"); err != nil {
		t.Fatalf("UntagImage: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("rootfs file still present after UntagImage: %v", err)
	}

	// Second call must succeed — eviction can race against catalogue
	// drift and a crash that left no artifact.
	if err := s.UntagImage(context.Background(), "sha256:cafe"); err != nil {
		t.Errorf("UntagImage idempotent call returned %v", err)
	}
}

func TestUntagImage_rejectsInvalidDigest(t *testing.T) {
	s := &Store{CacheDir: t.TempDir()}
	if err := s.UntagImage(context.Background(), ""); err == nil {
		t.Errorf("UntagImage(\"\") succeeded, want error")
	}
}

func TestNormalizeTarPath(t *testing.T) {
	good := map[string]string{
		"usr/bin/ls":     "/usr/bin/ls",
		"./etc/hostname": "/etc/hostname",
		"etc/":           "/etc",
		"./etc/":         "/etc",
		"a":              "/a",
	}
	for in, want := range good {
		got, ok := normalizeTarPath(in)
		if !ok {
			t.Errorf("normalizeTarPath(%q) returned ok=false", in)
			continue
		}
		if got != want {
			t.Errorf("normalizeTarPath(%q) = %q, want %q", in, got, want)
		}
	}

	// Archive root maps to empty string with ok=true so callers can
	// skip cleanly.
	for _, in := range []string{".", "/", "./"} {
		got, ok := normalizeTarPath(in)
		if !ok || got != "" {
			t.Errorf("normalizeTarPath(%q) = (%q, %v), want (\"\", true)", in, got, ok)
		}
	}

	bad := []string{
		"",
		"\x00",
		"foo\x00bar",
		"/abs/path",
		"../escape",
		"./../escape",
		"foo/../bar",
		"foo/./bar",
		"foo//bar",
	}
	for _, in := range bad {
		if _, ok := normalizeTarPath(in); ok {
			t.Errorf("normalizeTarPath(%q) returned ok=true, want false", in)
		}
	}
}

// TestBuildSquashfs_endToEnd writes a small synthetic OCI-style layer
// tar in memory, runs the production buildSquashfs pipeline against it,
// and confirms the result has the squashfs magic at offset 0 plus a
// non-empty inode count. We are not validating the entire image
// structure here — the squashfs writer's own tests do that — only the
// pipeline wiring: tar entries reach the writer, init + mountpoints get
// injected, and a sealed file lands on disk.
func TestBuildSquashfs_endToEnd(t *testing.T) {
	tarBytes := makeFixtureTar(t)
	out := filepath.Join(t.TempDir(), "rootfs.sqsh")
	initBin := []byte("\x7fELF FAKE-INIT BINARY")

	if err := buildSquashfs(bytes.NewReader(tarBytes), initBin, out); err != nil {
		t.Fatalf("buildSquashfs: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open rootfs: %v", err)
	}
	defer f.Close()

	var sb [96]byte
	if _, err := f.ReadAt(sb[:], 0); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	// "hsqs" little-endian — same magic the squashfs writer's own tests
	// check. Anything else means the file is not a squashfs.
	if got := binary.LittleEndian.Uint32(sb[0:4]); got != 0x73717368 {
		t.Errorf("magic = %#x, want 0x73717368", got)
	}
	if got := binary.LittleEndian.Uint32(sb[4:8]); got == 0 {
		t.Errorf("inode_count = 0, want >0")
	}
	bytesUsed := binary.LittleEndian.Uint64(sb[40:48])
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// Writer pads the file to a 4 KiB boundary so the kernel's
	// block-layer read of the last logical block can't short-read.
	if int64(bytesUsed) > info.Size() {
		t.Errorf("bytes_used = %d exceeds file size = %d", bytesUsed, info.Size())
	}
	if info.Size()%4096 != 0 {
		t.Errorf("file size = %d, want 4KiB-aligned", info.Size())
	}
}

// makeFixtureTar builds a small in-memory OCI-style layer tar with one
// of each entry type the pipeline supports. Returned bytes are safe to
// feed to buildSquashfs.
func makeFixtureTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Unix(1_700_000_000, 0)

	// Directory.
	mustWriteTarHeader(t, tw, &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "etc/",
		Mode:     0o755,
		ModTime:  mtime,
	})
	// Regular file.
	mustTarFile(t, tw, "etc/hostname", []byte("craftling\n"))
	// Symlink.
	mustWriteTarHeader(t, tw, &tar.Header{
		Typeflag: tar.TypeSymlink,
		Name:     "etc/host",
		Linkname: "hostname",
		Mode:     0o777,
		ModTime:  mtime,
	})
	// File the hardlink will target.
	mustTarFile(t, tw, "bin/sh", []byte("#!/bin/sh\nexit 0\n"))
	// Hardlink.
	mustWriteTarHeader(t, tw, &tar.Header{
		Typeflag: tar.TypeLink,
		Name:     "bin/ash",
		Linkname: "bin/sh",
		Mode:     0o755,
		ModTime:  mtime,
	})

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStreamTar_capsEntryCount asserts the entry-count cap fires before
// the writer accumulates unbounded inode metadata.
func TestStreamTar_capsEntryCount(t *testing.T) {
	withCaps(t, 1<<30, 4) // cap activates on the 5th entry

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i < 5; i++ {
		mustTarFile(t, tw, "f"+strconv.Itoa(i), []byte("x"))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	err := buildSquashfs(bytes.NewReader(buf.Bytes()), []byte("init"),
		filepath.Join(t.TempDir(), "out.sqsh"))
	if !errors.Is(err, ErrTarEntryCountExceeded) {
		t.Fatalf("got %v, want ErrTarEntryCountExceeded", err)
	}
}

// TestStreamTar_capsTotalBytesByHeader rejects a single fat entry whose
// declared size already overshoots the cap, without reading its body.
func TestStreamTar_capsTotalBytesByHeader(t *testing.T) {
	withCaps(t, 64, 1<<20)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteTarHeader(t, tw, &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "big",
		Size:     1024,
		Mode:     0o644,
	})
	if _, err := tw.Write(bytes.Repeat([]byte{0}, 1024)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	err := buildSquashfs(bytes.NewReader(buf.Bytes()), []byte("init"),
		filepath.Join(t.TempDir(), "out.sqsh"))
	if !errors.Is(err, ErrTarTotalBytesExceeded) {
		t.Fatalf("got %v, want ErrTarTotalBytesExceeded", err)
	}
}

// TestStreamTar_capsTotalBytesByBody catches the cumulative case — two
// entries that each fit under the cap individually but cross it
// together.
func TestStreamTar_capsTotalBytesByBody(t *testing.T) {
	withCaps(t, 32, 1<<20)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustTarFile(t, tw, "a", bytes.Repeat([]byte("a"), 20))
	mustTarFile(t, tw, "b", bytes.Repeat([]byte("b"), 20))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	err := buildSquashfs(bytes.NewReader(buf.Bytes()), []byte("init"),
		filepath.Join(t.TempDir(), "out.sqsh"))
	if !errors.Is(err, ErrTarTotalBytesExceeded) {
		t.Fatalf("got %v, want ErrTarTotalBytesExceeded", err)
	}
}

// withCaps temporarily swaps the package-level cap vars so a test can
// drive the cap path with byte-sized fixtures.
func withCaps(t *testing.T, totalBytes, entryCount int64) {
	t.Helper()
	prevBytes, prevEntries := maxTarTotalBytes, maxTarEntryCount
	maxTarTotalBytes = totalBytes
	maxTarEntryCount = entryCount
	t.Cleanup(func() {
		maxTarTotalBytes = prevBytes
		maxTarEntryCount = prevEntries
	})
}

func mustTarFile(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	mustWriteTarHeader(t, tw, &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		ModTime:  time.Unix(1_700_000_000, 0),
	})
	if _, err := io.Copy(tw, bytes.NewReader(body)); err != nil {
		t.Fatalf("write tar body %q: %v", name, err)
	}
}

func mustWriteTarHeader(t *testing.T, tw *tar.Writer, h *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(h); err != nil {
		t.Fatalf("write tar header %q: %v", h.Name, err)
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
