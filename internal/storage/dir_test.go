package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const id = "srv-1"
	if ok, err := s.Exists(ctx, id); err != nil || ok {
		t.Fatalf("Exists on empty store = %v, %v; want false, nil", ok, err)
	}
	if _, err := s.Get(ctx, id); !errors.Is(err, ErrWorldNotFound) {
		t.Fatalf("Get on empty store = %v; want ErrWorldNotFound", err)
	}

	payload := []byte("a world snapshot")
	if err := s.Put(ctx, id, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := s.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("Exists after Put = %v, %v; want true, nil", ok, err)
	}

	rc, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	// Put replaces the prior snapshot rather than appending.
	if err := s.Put(ctx, id, strings.NewReader("newer")); err != nil {
		t.Fatalf("Put replace: %v", err)
	}
	rc, _ = s.Get(ctx, id)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "newer" {
		t.Errorf("after replace Get = %q, want %q", got, "newer")
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Exists(ctx, id); ok {
		t.Error("Exists after Delete = true")
	}
	// Delete is idempotent.
	if err := s.Delete(ctx, id); err != nil {
		t.Errorf("second Delete = %v; want nil", err)
	}
}

// TestDirStoreKeyIsContained checks an adversarial server id can't write outside
// the store root.
func TestDirStoreKeyIsContained(t *testing.T) {
	root := t.TempDir()
	s, err := NewDirStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "../../escape", strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The blob lands directly under root, name sanitized — nothing escaped.
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry under root, got %d", len(entries))
	}
	if strings.ContainsRune(entries[0].Name(), os.PathSeparator) {
		t.Errorf("entry name has a separator: %q", entries[0].Name())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.world")); err == nil {
		t.Error("an escaped blob was written outside the store root")
	}
}

// TestDirStorePutNoPartialOnError ensures a read error mid-Put leaves no blob
// (the temp file is removed, not renamed into place).
func TestDirStorePutNoPartialOnError(t *testing.T) {
	root := t.TempDir()
	s, err := NewDirStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "srv", errReader{}); err == nil {
		t.Fatal("expected Put to fail on a reader error")
	}
	if ok, _ := s.Exists(context.Background(), "srv"); ok {
		t.Error("a partial snapshot was published despite the read error")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

// errReader is a reader that always fails, to exercise Put's error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
