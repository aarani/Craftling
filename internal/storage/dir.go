package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DirStore is a WorldStore backed by a directory on the host filesystem. It is
// the simplest durable backend: point it at a shared mount (NFS, a SAN volume)
// and several agents see the same worlds, which is enough for cross-host
// reschedule without standing up object storage. It is also what the tests use,
// since it needs nothing but a temp dir. An S3-backed store implements the same
// interface for cloud deployments.
type DirStore struct {
	root string
}

// compile-time check.
var _ WorldStore = (*DirStore)(nil)

// NewDirStore returns a DirStore writing under root, creating it if needed.
func NewDirStore(root string) (*DirStore, error) {
	if root == "" {
		return nil, fmt.Errorf("storage: DirStore root is required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create world dir %q: %w", root, err)
	}
	return &DirStore{root: root}, nil
}

// path is the on-disk blob path for a server id.
func (s *DirStore) path(serverID string) string {
	return filepath.Join(s.root, SafeKey(serverID)+WorldSuffix)
}

// Exists reports whether a snapshot file is present for serverID.
func (s *DirStore) Exists(_ context.Context, serverID string) (bool, error) {
	_, err := os.Stat(s.path(serverID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("storage: stat world %q: %w", serverID, err)
}

// Put writes r to a sibling temp file and atomically renames it into place, so
// a crash or read error mid-stream never leaves a truncated snapshot that a
// later Get would treat as whole.
func (s *DirStore) Put(_ context.Context, serverID string, r io.Reader) error {
	final := s.path(serverID)
	tmp, err := os.CreateTemp(s.root, SafeKey(serverID)+".*.tmp")
	if err != nil {
		return fmt.Errorf("storage: create temp world: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: write world %q: %w", serverID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close world %q: %w", serverID, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("storage: publish world %q: %w", serverID, err)
	}
	return nil
}

// Get opens the snapshot for serverID, mapping a missing file to
// ErrWorldNotFound.
func (s *DirStore) Get(_ context.Context, serverID string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(serverID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrWorldNotFound
		}
		return nil, fmt.Errorf("storage: open world %q: %w", serverID, err)
	}
	return f, nil
}

// Delete removes the snapshot for serverID; a missing file is success.
func (s *DirStore) Delete(_ context.Context, serverID string) error {
	if err := os.Remove(s.path(serverID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete world %q: %w", serverID, err)
	}
	return nil
}
