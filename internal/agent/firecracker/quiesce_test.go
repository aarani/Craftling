package firecracker

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/storage"
)

// fakeGuestVsock stands in for Firecracker's host-side vsock UDS plus the guest
// control server: it completes the CONNECT handshake, then answers PREPARE and
// RESUME with OK, recording the order it saw them. It lets us test the host
// snapshot orchestration without a real VM.
type fakeGuestVsock struct {
	ln       net.Listener
	commands chan string
}

func newFakeGuestVsock(t *testing.T, uds string) *fakeGuestVsock {
	t.Helper()
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatalf("listen fake vsock: %v", err)
	}
	g := &fakeGuestVsock{ln: ln, commands: make(chan string, 8)}
	go g.serve()
	return g
}

func (g *fakeGuestVsock) serve() {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			// Firecracker handshake: "CONNECT <port>\n" → "OK <n>\n".
			if _, err := br.ReadString('\n'); err != nil {
				return
			}
			_, _ = conn.Write([]byte("OK 0\n"))
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				cmd := line[:len(line)-1]
				g.commands <- cmd
				_, _ = conn.Write([]byte("OK\n"))
			}
		}()
	}
}

func (g *fakeGuestVsock) close() { _ = g.ln.Close() }

// shortSockPath returns a unix-socket path under a short temp dir. The OS caps
// sun_path (~104 bytes on macOS), which t.TempDir's long, test-name-derived
// paths can exceed.
func shortSockPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "v")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "v.sock")
}

// TestSnapshotRunningOrchestratesFreezeAndStore checks the host PREPARE →
// snapshot → RESUME exchange: the world is stored, and PREPARE precedes RESUME
// (so the disk is read while "frozen").
func TestSnapshotRunningOrchestratesFreezeAndStore(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	uds := shortSockPath(t)
	guest := newFakeGuestVsock(t, uds)
	defer guest.close()

	disk := filepath.Join(dir, "world.ext4")
	const content = "WORLD-DISK-CONTENT-while-frozen"
	if err := os.WriteFile(disk, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}

	rt := newTestRuntime(t)
	rt.store = store
	m := &machine{id: "vm-x", serverID: "srv", worldKey: "srv", worldDisk: disk, vsockUDS: uds}

	if err := rt.snapshotRunning(ctx, m); err != nil {
		t.Fatalf("snapshotRunning: %v", err)
	}

	// Order: PREPARE then RESUME.
	if c := <-guest.commands; c != "PREPARE" {
		t.Fatalf("first command = %q, want PREPARE", c)
	}
	if c := <-guest.commands; c != "RESUME" {
		t.Fatalf("second command = %q, want RESUME", c)
	}

	// The world landed in the store and round-trips back to the disk content.
	if ok, _ := store.Exists(ctx, "srv"); !ok {
		t.Fatal("snapshot not stored")
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := restoreWorldDisk(ctx, store, "srv", restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(restored)
	if string(got) != content {
		t.Errorf("restored = %q, want %q", got, content)
	}
}

// TestSnapshotUnknownVM checks the on-demand Snapshot RPC maps an unknown id to
// agent.ErrVMNotFound (so the HTTP layer can return 404).
func TestSnapshotUnknownVM(t *testing.T) {
	rt := newTestRuntime(t)
	if err := rt.Snapshot(context.Background(), "vm-ghost"); !errors.Is(err, agent.ErrVMNotFound) {
		t.Fatalf("Snapshot unknown = %v, want ErrVMNotFound", err)
	}
}

// TestSnapshotUnavailableWithoutStore checks Snapshot errors (rather than
// silently succeeding) when the VM exists but live snapshots aren't configured.
func TestSnapshotUnavailableWithoutStore(t *testing.T) {
	rt := newTestRuntime(t) // no store, no vsock
	m := &machine{id: "vm-x", worldDisk: "/tmp/x", vsockUDS: ""}
	rt.mu.Lock()
	rt.vms[m.id] = m
	rt.mu.Unlock()
	if err := rt.Snapshot(context.Background(), m.id); err == nil {
		t.Fatal("expected Snapshot to fail when no store is configured")
	}
}

// TestSnapshotRunningResumesOnSnapshotFailure verifies the guest is always
// thawed (RESUME sent) even when the snapshot step fails — here the disk path
// is missing, so the store copy fails after PREPARE.
func TestSnapshotRunningResumesOnSnapshotFailure(t *testing.T) {
	ctx := context.Background()
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	uds := shortSockPath(t)
	guest := newFakeGuestVsock(t, uds)
	defer guest.close()

	rt := newTestRuntime(t)
	rt.store = store
	m := &machine{id: "vm-x", worldKey: "srv", worldDisk: filepath.Join(dir, "missing.ext4"), vsockUDS: uds}

	if err := rt.snapshotRunning(ctx, m); err == nil {
		t.Fatal("expected snapshot to fail on a missing disk")
	}
	if c := <-guest.commands; c != "PREPARE" {
		t.Fatalf("first command = %q, want PREPARE", c)
	}
	if c := <-guest.commands; c != "RESUME" {
		t.Fatalf("second command = %q, want RESUME (thaw must run even on failure)", c)
	}
}
