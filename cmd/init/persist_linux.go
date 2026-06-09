//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/aarani/craftling-go/internal/runspec"
)

// persistScratch is where the world disk is mounted before it is overlaid
// onto WorkingDir. It lives under /run, a tmpfs the agent mounts in setupInit,
// so we can create the mountpoint even though the rootfs is read-only — the
// ext4 mount then shadows the tmpfs dir with the writable disk.
const persistScratch = "/run/craftling/persist"

// applyPersist makes WorkingDir durable by overlaying the host-attached world
// disk onto it (P5a). The read-only image content stays visible (it is the
// overlay lowerdir) while every write — the Minecraft world, logs, edited
// configs — lands on the disk, which the host can snapshot.
//
//  1. mount the ext4 world disk at a /run tmpfs scratch dir
//  2. create upper/ and work/ on it (overlayfs needs both, same fs)
//  3. mount -t overlay with lowerdir=Mountpoint back onto Mountpoint
//
// lowerdir is resolved by the kernel before the overlay shadows the path, so
// pointing it at the same directory we mount over is the standard self-overlay
// pattern, not a cycle.
func applyPersist(p *runspec.PersistConfig) error {
	if p.Device == "" || p.Mountpoint == "" {
		return fmt.Errorf("incomplete persist config: device=%q mountpoint=%q", p.Device, p.Mountpoint)
	}

	if err := os.MkdirAll(persistScratch, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", persistScratch, err)
	}
	if err := syscall.Mount(p.Device, persistScratch, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s at %s: %w", p.Device, persistScratch, err)
	}

	upper := persistScratch + "/upper"
	work := persistScratch + "/work"
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", p.Mountpoint, upper, work)
	if err := syscall.Mount("overlay", p.Mountpoint, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay mount on %s: %w", p.Mountpoint, err)
	}
	return nil
}
