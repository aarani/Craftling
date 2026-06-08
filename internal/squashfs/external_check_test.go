package squashfs

import (
	"bytes"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// External-validator tests: feed an image written by our Writer to the
// `unsquashfs` binary (squashfs-tools) and confirm the kernel's
// canonical reader implementation accepts it. This is the test that
// catches on-wire format regressions our self-validating unit tests
// can't see. Skips gracefully when unsquashfs isn't installed, so the
// macOS-only developer workflow stays green.
//
// Distro packages: Debian/Ubuntu = squashfs-tools, Homebrew = squashfs.

// hasUnsquashfs reports whether the unsquashfs binary is on PATH. The
// test skips itself otherwise.
func hasUnsquashfs(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("unsquashfs")
	if err != nil {
		t.Skip("unsquashfs not installed; install squashfs-tools (apt) / squashfs (brew) to run")
	}
	return p
}

// TestRoundTripViaUnsquashfs exercises every entry kind through the
// writer, then feeds the result to unsquashfs and inspects the
// extracted tree. The fixture is deliberately broad: a multi-block
// regular file, an empty file, an empty directory, a deep tree, a long
// filename, a hardlink, and a symlink. Anything we add to the Writer's
// API should land an entry here too.
func TestRoundTripViaUnsquashfs(t *testing.T) {
	bin := hasUnsquashfs(t)

	out := filepath.Join(t.TempDir(), "image.sqsh")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(f, WithCompressor(StoredCompressor{}))
	if err != nil {
		f.Close()
		t.Fatal(err)
	}

	mtime := time.Unix(1_700_000_000, 0)
	attrs := func(p string, mode fs.FileMode) Attrs {
		return Attrs{Path: p, Mode: mode, UID: 1000, GID: 1000, Mtime: mtime}
	}

	// Top-level directories.
	must(t, w.CreateDir(attrs("/etc", 0o755)))
	must(t, w.CreateDir(attrs("/bin", 0o755)))
	must(t, w.CreateDir(attrs("/empty", 0o755)))
	// Deep tree — exercises parent inode chains.
	must(t, w.CreateDir(attrs("/a", 0o755)))
	must(t, w.CreateDir(attrs("/a/b", 0o755)))
	must(t, w.CreateDir(attrs("/a/b/c", 0o755)))
	must(t, w.CreateDir(attrs("/a/b/c/d", 0o755)))

	// Empty regular file — exercises the file inode when no data blocks
	// are written.
	fw, err := w.CreateFile(attrs("/etc/empty", 0o644))
	must(t, err)
	_ = fw

	// Small regular file.
	fw, err = w.CreateFile(attrs("/etc/hostname", 0o644))
	must(t, err)
	writeAll(t, fw, []byte("craftling-test\n"))

	// Multi-block file — exercises the block_sizes array spanning
	// several entries. 350 KiB at 128 KiB block size = 3 blocks (two
	// full, one short tail).
	multiBlockBody := bytes.Repeat([]byte("ABCDEFGH"), 350*1024/8)
	fw, err = w.CreateFile(attrs("/bin/multi", 0o755))
	must(t, err)
	writeAll(t, fw, multiBlockBody)

	// File whose name is at the format's per-entry limit (256 bytes of
	// name; the on-wire name_size field stores len(name)-1).
	longName := "/etc/" + strings.Repeat("L", 252)
	fw, err = w.CreateFile(attrs(longName, 0o644))
	must(t, err)
	writeAll(t, fw, []byte("long-name-payload"))

	// Symlink.
	must(t, w.CreateSymlink(attrs("/etc/host", 0o777), "hostname"))

	// Hardlink — must share the target's inode.
	must(t, w.CreateHardlink("/bin/multi-link", "/bin/multi"))

	// Deep-tree leaf so the post-order dir walk exercises a nontrivial
	// branch.
	fw, err = w.CreateFile(attrs("/a/b/c/d/leaf", 0o644))
	must(t, err)
	writeAll(t, fw, []byte("leaf"))

	must(t, w.Close())
	must(t, f.Close())

	// Step 1: listing — quickest signal that the superblock and
	// directory tables parse.
	listing := mustRun(t, exec.Command(bin, "-ll", "-no-progress", out))
	for _, want := range []string{
		"squashfs-root/etc",
		"squashfs-root/etc/hostname",
		"squashfs-root/etc/host -> hostname",
		"squashfs-root/bin/multi",
		"squashfs-root/bin/multi-link",
		"squashfs-root/a/b/c/d/leaf",
		"squashfs-root/empty",
	} {
		if !strings.Contains(listing, want) {
			t.Errorf("unsquashfs -ll missing %q in:\n%s", want, listing)
		}
	}

	// Step 2: extract and verify content + structure.
	extractDir := filepath.Join(t.TempDir(), "extracted")
	mustRun(t, exec.Command(bin, "-d", extractDir, "-no-progress", out))

	// Content checks.
	checkContent(t, filepath.Join(extractDir, "etc/hostname"), "craftling-test\n")
	checkContent(t, filepath.Join(extractDir, "etc/empty"), "")
	checkContent(t, filepath.Join(extractDir, "a/b/c/d/leaf"), "leaf")
	checkContent(t, filepath.Join(extractDir, "etc", strings.Repeat("L", 252)), "long-name-payload")

	// Multi-block content: compare sha256 instead of holding the whole
	// 350 KiB in the test output on mismatch.
	gotBytes, err := os.ReadFile(filepath.Join(extractDir, "bin/multi"))
	if err != nil {
		t.Fatalf("read multi: %v", err)
	}
	if gotSum, wantSum := sha256.Sum256(gotBytes), sha256.Sum256(multiBlockBody); gotSum != wantSum {
		t.Errorf("multi-block file sha256 mismatch: got %x want %x", gotSum, wantSum)
	}

	// Hardlink sharing: the two names must report the same inode.
	multiIno := statIno(t, filepath.Join(extractDir, "bin/multi"))
	linkIno := statIno(t, filepath.Join(extractDir, "bin/multi-link"))
	if multiIno != linkIno {
		t.Errorf("hardlink inode mismatch: /bin/multi=%d /bin/multi-link=%d", multiIno, linkIno)
	}

	// Symlink target round-trip.
	got, err := os.Readlink(filepath.Join(extractDir, "etc/host"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if got != "hostname" {
		t.Errorf("symlink target = %q, want %q", got, "hostname")
	}

	// Empty directory must extract cleanly with no entries.
	entries, err := os.ReadDir(filepath.Join(extractDir, "empty"))
	if err != nil {
		t.Fatalf("read /empty: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("/empty has %d entries, want 0", len(entries))
	}
}

// TestUnsquashfsStatVerifiesSuperblock runs `unsquashfs -stat`, the
// tool's superblock dump command, and confirms it parses our header
// without complaint.
func TestUnsquashfsStatVerifiesSuperblock(t *testing.T) {
	bin := hasUnsquashfs(t)

	out := filepath.Join(t.TempDir(), "image.sqsh")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(f, WithCompressor(StoredCompressor{}))
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	must(t, w.CreateDir(Attrs{Path: "/x", Mode: 0o755, Mtime: time.Unix(1_700_000_000, 0)}))
	must(t, w.Close())
	must(t, f.Close())

	statOut := mustRun(t, exec.Command(bin, "-stat", out))
	// Superblock version (4.0) and the expected total inode count for
	// the synthesized root + /x.
	for _, want := range []string{"SQUASHFS 4:0 superblock", "Number of inodes 2"} {
		if !strings.Contains(statOut, want) {
			t.Errorf("unsquashfs -stat missing %q in:\n%s", want, statOut)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeAll(t *testing.T, w io.Writer, p []byte) {
	t.Helper()
	if _, err := w.Write(p); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustRun(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
	}
	return string(out)
}

func checkContent(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(p)
	if err != nil {
		t.Errorf("read %s: %v", p, err)
		return
	}
	if string(got) != want {
		t.Errorf("%s content = %q, want %q", p, got, want)
	}
}

// statIno returns the underlying inode number for a path. Used to
// confirm hardlinks share a single inode after extraction.
func statIno(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no Stat_t (platform without inode reporting?)", p)
	}
	return uint64(sys.Ino)
}
