package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// World disk lifecycle (P5a). A world disk is a per-server ext4 file the host
// creates once and attaches to the VM as a second virtio-blk device; the guest
// overlays it onto the workload's WorkingDir so the Minecraft world survives a
// stop/start (the squashfs rootfs is read-only and everything else is tmpfs).
// It lives under Config.DataDir keyed by server id, separate from the per-VM
// WorkDir that Deprovision wipes, so it can outlive a single VM instance.

// worldDiskPath returns the on-disk path of the world disk for a server. key is
// the server id (falling back to the VM id when a spec carries no server id);
// it is sanitized to a safe single path segment so an id can never escape
// DataDir.
func (c *Config) worldDiskPath(key string) string {
	return filepath.Join(c.DataDir, sanitizeKey(key), "world.ext4")
}

// sanitizeKey maps an arbitrary id to a single filesystem-safe path segment,
// replacing anything outside [A-Za-z0-9._-] with '_'. An empty or all-unsafe
// id collapses to "_", which is harmless: the caller only ever passes a UUID
// or server id, and this is a defense-in-depth guard, not the namespace.
func sanitizeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// ensureWorldDisk makes sure a formatted world disk exists at path, creating
// and mkfs.ext4-ing it on first call. An already-present disk is left untouched
// — that is how a world persists across stop/start (and, later, a restore that
// seeds the disk before boot): a second call is a no-op, never a reformat.
//
// The disk is created as a sparse file truncated to sizeMB, so mkfs lays down a
// filesystem of that size without the host committing the bytes upfront.
func ensureWorldDisk(path string, sizeMB int, mkfsPath string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already provisioned; keep the existing world
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat world disk %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("world disk dir: %w", err)
	}

	// Build against a sibling tmp path and rename into place, so a crash
	// mid-mkfs never leaves a half-formatted file that the stat above would
	// mistake for a finished disk on the next provision.
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640) //nolint:gosec // driver-controlled path
	if err != nil {
		return fmt.Errorf("create world disk: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) << 20); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("size world disk: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close world disk: %w", err)
	}

	// -F: don't prompt on a plain file; -q: quiet. mkfs detects the size from
	// the truncated file.
	cmd := exec.Command(mkfsPath, "-F", "-q", tmp) //nolint:gosec // mkfsPath is operator-configured, tmp is driver-controlled
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs.ext4 world disk: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publish world disk: %w", err)
	}
	return nil
}

// persistTarget validates a workload WorkingDir as an overlay mountpoint and
// returns it. Persistence overlays the world disk onto WorkingDir, so it must
// be an absolute, non-root path: "/" or "" has no meaningful lowerdir to union
// and overlaying the whole root filesystem is unsafe.
func persistTarget(workingDir string) (string, bool) {
	if workingDir == "" || workingDir == "/" || !strings.HasPrefix(workingDir, "/") {
		return "", false
	}
	return workingDir, true
}
