package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aarani/craftling-go/internal/storage"
)

// TestWorldSnapshotRoundTrip snapshots a disk image into a store and restores it
// to a new path, asserting the bytes survive the gzip codec intact.
func TestWorldSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "world.ext4")
	// A payload with both incompressible and zero regions, like a real ext4
	// image: random-ish header followed by a big run of holes.
	content := append([]byte("EXT4-LIKE-HEADER-bytes-0123456789"), make([]byte, 1<<16)...)
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := snapshotWorldDisk(ctx, store, "srv-1", src); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	dst := filepath.Join(dir, "restored.ext4")
	if err := restoreWorldDisk(ctx, store, "srv-1", dst); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("restored disk differs from source (len got=%d want=%d)", len(got), len(content))
	}
}

// TestPrepareWorldDiskRestoresFromStore checks Provision's disk-prep step pulls
// an existing world from the store instead of formatting a fresh one. The mkfs
// path is bogus on purpose: if the restore branch didn't short-circuit, the
// fresh-format fallback would try to run it and fail.
func TestPrepareWorldDiskRestoresFromStore(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Seed the store with a snapshot of a sentinel "disk".
	seedDir := t.TempDir()
	seed := filepath.Join(seedDir, "world.ext4")
	const sentinel = "RESTORED-WORLD-CONTENT"
	if err := os.WriteFile(seed, []byte(sentinel), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := snapshotWorldDisk(ctx, store, "srv-1", seed); err != nil {
		t.Fatal(err)
	}

	rt := newTestRuntime(t)
	rt.store = store
	rt.cfg.MkfsExt4Path = "/nonexistent/mkfs.ext4" // must NOT be invoked on the restore path

	disk := filepath.Join(t.TempDir(), "srv", "world.ext4")
	if err := rt.prepareWorldDisk(ctx, "srv-1", disk); err != nil {
		t.Fatalf("prepareWorldDisk: %v", err)
	}
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Errorf("prepared disk = %q, want restored %q", got, sentinel)
	}
}

// TestPrepareWorldDiskFreshWhenStoreEmpty checks that with no stored world the
// prep falls through to formatting a fresh disk (here the bogus mkfs makes that
// fallback observable as an error rather than a silent restore).
func TestPrepareWorldDiskFreshWhenStoreEmpty(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := newTestRuntime(t)
	rt.store = store
	rt.cfg.WorldDiskMB = 64
	rt.cfg.MkfsExt4Path = "/nonexistent/mkfs.ext4"

	disk := filepath.Join(t.TempDir(), "srv", "world.ext4")
	if err := rt.prepareWorldDisk(ctx, "srv-1", disk); err == nil {
		t.Fatal("expected fresh-format fallback to invoke mkfs and fail")
	}
}
