package image

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aarani/craftling-go/internal/runspec"
)

// External-validator tests for the production buildSquashfs pipeline.
// Skip when unsquashfs is absent so macOS-only developer machines don't
// hard-fail; CI installs squashfs-tools and runs these for real,
// catching format regressions long before the firecracker e2e job tries
// to boot a kernel against a broken rootfs.

// TestBuildSquashfs_externalRoundTrip drives the production buildSquashfs
// function with the same fixture tar used by the in-process test,
// extracts the result with unsquashfs, and asserts tree contents, init
// injection, hardlink-shares-inode, and the standard mountpoint dirs.
func TestBuildSquashfs_externalRoundTrip(t *testing.T) {
	bin, err := exec.LookPath("unsquashfs")
	if err != nil {
		t.Skip("unsquashfs not installed; install squashfs-tools to run")
	}

	tarBytes := makeFixtureTar(t)
	out := filepath.Join(t.TempDir(), "rootfs.sqsh")
	initMarker := []byte("REAL-CRAFTLING-INIT-MARKER\n")

	if err := buildSquashfs(bytes.NewReader(tarBytes), initMarker, out); err != nil {
		t.Fatalf("buildSquashfs: %v", err)
	}

	extracted := filepath.Join(t.TempDir(), "extract")
	if combined, err := exec.Command(bin, "-d", extracted, "-no-progress", out).CombinedOutput(); err != nil {
		t.Fatalf("unsquashfs failed: %v\n%s", err, combined)
	}

	// User-supplied entries round-trip with correct contents.
	checkFile(t, filepath.Join(extracted, "etc/hostname"), []byte("craftling\n"))
	checkFile(t, filepath.Join(extracted, "bin/sh"), []byte("#!/bin/sh\nexit 0\n"))

	// Init injection lands the right bytes at /.craftling/init.
	checkFile(t, filepath.Join(extracted, strings.TrimPrefix(runspec.InitPath, "/")), initMarker)

	// Standard mountpoints exist as directories. The init agent mounts
	// over them; the on-disk perms don't matter for runtime behaviour
	// but the entries must be present so mount(2) succeeds against a
	// read-only rootfs.
	for _, mp := range []string{"proc", "sys", "dev", "tmp", "run"} {
		info, err := os.Stat(filepath.Join(extracted, mp))
		if err != nil {
			t.Errorf("mountpoint /%s missing: %v", mp, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("mountpoint /%s is not a directory", mp)
		}
	}

	// /tmp gets the sticky-world-writable mode in the writer. Check the
	// IMAGE's listing rather than the extracted dir because unsquashfs
	// running as a non-root user can't always apply sticky bits.
	listing, err := exec.Command(bin, "-ll", "-no-progress", out).CombinedOutput()
	if err != nil {
		t.Fatalf("unsquashfs -ll: %v\n%s", err, listing)
	}
	if !strings.Contains(string(listing), "drwxrwxrwt") {
		t.Errorf("listing missing sticky-world-writable /tmp entry:\n%s", listing)
	}

	// Hardlink shares the inode of its target — the squashfs hardlink
	// encoding's "single-inode" promise.
	if got := pipelineStatIno(t, filepath.Join(extracted, "bin/sh")); got != pipelineStatIno(t, filepath.Join(extracted, "bin/ash")) {
		t.Errorf("/bin/sh and /bin/ash should share an inode; got distinct values")
	}

	// Symlink target round-trip.
	target, err := os.Readlink(filepath.Join(extracted, "etc/host"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "hostname" {
		t.Errorf("/etc/host -> %q, want %q", target, "hostname")
	}
}

// TestBuildSquashfs_externalStripsCraftlingNamespace confirms that
// user-supplied /.craftling bytes from a hostile image are NOT visible
// after extraction — the injected init is. Validated through the
// canonical reader rather than byte-substring scanning on the raw
// archive.
func TestBuildSquashfs_externalStripsCraftlingNamespace(t *testing.T) {
	bin, err := exec.LookPath("unsquashfs")
	if err != nil {
		t.Skip("unsquashfs not installed; install squashfs-tools to run")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustTarFile(t, tw, strings.TrimPrefix(runspec.InitPath, "/"), []byte("USER-EVIL-INIT"))
	mustTarFile(t, tw, "etc/hostname", []byte("craftling\n"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "rootfs.sqsh")
	realInit := []byte("REAL-CRAFTLING-INIT-MARKER\n")
	if err := buildSquashfs(&buf, realInit, out); err != nil {
		t.Fatalf("buildSquashfs: %v", err)
	}

	extracted := filepath.Join(t.TempDir(), "extract")
	if combined, err := exec.Command(bin, "-d", extracted, "-no-progress", out).CombinedOutput(); err != nil {
		t.Fatalf("unsquashfs: %v\n%s", err, combined)
	}

	got, err := os.ReadFile(filepath.Join(extracted, strings.TrimPrefix(runspec.InitPath, "/")))
	if err != nil {
		t.Fatalf("read %s: %v", runspec.InitPath, err)
	}
	if bytes.Equal(got, []byte("USER-EVIL-INIT")) {
		t.Errorf("hostile user-supplied %s reached the extracted rootfs", runspec.InitPath)
	}
	if !bytes.Equal(got, realInit) {
		t.Errorf("%s = %q, want %q", runspec.InitPath, got, realInit)
	}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	if !bytes.Equal(got, want) {
		gs, ws := string(got), string(want)
		if len(gs) > 200 {
			gs = gs[:200] + "..."
		}
		if len(ws) > 200 {
			ws = ws[:200] + "..."
		}
		t.Errorf("%s content mismatch: got %q want %q", path, gs, ws)
	}
}

// pipelineStatIno returns the underlying inode number for a path, for
// hardlink-shared-inode assertions.
func pipelineStatIno(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: platform lacks inode reporting", p)
	}
	return uint64(sys.Ino)
}
