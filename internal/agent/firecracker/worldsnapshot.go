package firecracker

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aarani/craftling-go/internal/storage"
)

// World snapshot codec (P5b): the bytes that move between a per-server world
// disk (a raw ext4 image file on the host) and the WorldStore. The image is
// gzip-compressed in flight — an ext4 with a small world is mostly zeroed
// blocks, which gzip collapses to almost nothing, so we get cheap snapshots
// without teaching the store about filesystems or sparseness.
//
// These snapshots are crash-consistent: they are taken while the VM is powered
// down (Stop shuts the guest off first, so it has synced), which is correct for
// a stopped server. Application-consistent snapshots of a *running* server
// (RCON flush + fsfreeze) are P5c.

// snapshotWorldDisk compresses the world disk at diskPath and stores it under
// serverID. The caller must ensure the VM is not writing the disk (powered off)
// before calling, or the image may be torn.
func snapshotWorldDisk(ctx context.Context, store storage.WorldStore, serverID, diskPath string) error {
	f, err := os.Open(diskPath) //nolint:gosec // driver-controlled path
	if err != nil {
		return fmt.Errorf("open world disk: %w", err)
	}
	defer func() { _ = f.Close() }()

	// gzip the file into a pipe the store reads from, so nothing is buffered
	// whole in memory. A read/compress error is surfaced to the store's Copy
	// via CloseWithError, which fails Put — no partial snapshot is published.
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		_, err := io.Copy(gz, f)
		if err == nil {
			err = gz.Close()
		}
		_ = pw.CloseWithError(err)
	}()

	if err := store.Put(ctx, serverID, pr); err != nil {
		_ = pr.CloseWithError(err) // unblock the goroutine if Put bailed early
		return fmt.Errorf("store world snapshot: %w", err)
	}
	return nil
}

// restoreWorldDisk downloads serverID's snapshot, decompresses it, and writes it
// to diskPath, replacing whatever is there. It builds a sibling temp file and
// renames into place so an interrupted restore never leaves a half-written disk
// that the next boot would mount as a corrupt world.
func restoreWorldDisk(ctx context.Context, store storage.WorldStore, serverID, diskPath string) error {
	rc, err := store.Get(ctx, serverID)
	if err != nil {
		return fmt.Errorf("get world snapshot: %w", err)
	}
	defer func() { _ = rc.Close() }()

	gz, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("open world snapshot: %w", err)
	}
	defer func() { _ = gz.Close() }()

	if err := os.MkdirAll(filepath.Dir(diskPath), 0o750); err != nil {
		return fmt.Errorf("world disk dir: %w", err)
	}
	tmp := diskPath + ".restore"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640) //nolint:gosec // driver-controlled path
	if err != nil {
		return fmt.Errorf("create world disk: %w", err)
	}
	if _, err := io.Copy(out, gz); err != nil { //nolint:gosec // size bounded by the disk we wrote
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write world disk: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close world disk: %w", err)
	}
	if err := os.Rename(tmp, diskPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish world disk: %w", err)
	}
	return nil
}
