package provisioner

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/model"
	"go.uber.org/zap"
)

// stubResolver resolves every host id to a fixed agent address.
type stubResolver struct{ addr string }

func (s stubResolver) GetByID(_ context.Context, id string) (*model.Host, error) {
	return &model.Host{ID: id, Address: s.addr}, nil
}

func ptr(s string) *string { return &s }

// TestRemoteProvisionerLifecycle drives a game server through provision → stop →
// start → deprovision against a real in-process agent, asserting the observed
// state reported back across the seam at each step.
func TestRemoteProvisionerLifecycle(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(agent.NewRouter(agent.NewFakeRuntime("10.0.0.20"), zap.NewNop()))
	defer srv.Close()

	p := NewRemote(stubResolver{addr: srv.URL}, agent.NewClient(nil))
	s := &model.GameServer{
		ID:       "srv-1",
		HostID:   ptr("host-1"),
		Game:     "minecraft",
		Version:  "1.20.4",
		CPUs:     2,
		MemoryMB: 2048,
	}

	inst, err := p.Provision(ctx, s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if inst.VMID == "" || inst.Host != "10.0.0.20" || inst.Port != 25565 {
		t.Fatalf("instance = %+v, want vmid set and 10.0.0.20:25565", inst)
	}
	s.VMID = &inst.VMID

	assertRemoteState(t, p, s, StateRunning)

	if err := p.Stop(ctx, s); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertRemoteState(t, p, s, StateStopped)

	if _, err := p.Start(ctx, s); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRemoteState(t, p, s, StateRunning)

	if err := p.Deprovision(ctx, s); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	assertRemoteState(t, p, s, StateMissing)
}

// TestRemoteProvisionerUnplaced verifies provisioning without a host assignment
// is a logic error, while teardown of an unplaced/unprovisioned server is a
// harmless no-op.
func TestRemoteProvisionerUnplaced(t *testing.T) {
	ctx := context.Background()
	p := NewRemote(stubResolver{addr: "http://127.0.0.1:1"}, agent.NewClient(nil))

	if _, err := p.Provision(ctx, &model.GameServer{ID: "x"}); !errors.Is(err, ErrUnplaced) {
		t.Errorf("provision unplaced = %v, want ErrUnplaced", err)
	}
	// No host and no VM: nothing to tear down, and we must not dial anyone.
	if err := p.Deprovision(ctx, &model.GameServer{ID: "x"}); err != nil {
		t.Errorf("deprovision unplaced = %v, want nil", err)
	}
	if err := p.Stop(ctx, &model.GameServer{ID: "x", HostID: ptr("h")}); err != nil {
		t.Errorf("stop with no vm = %v, want nil", err)
	}
}

// TestRemoteProvisionerStartProvisions verifies Start with no recorded VM falls
// back to provisioning a fresh one.
func TestRemoteProvisionerStartProvisions(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(agent.NewRouter(agent.NewFakeRuntime("10.0.0.21"), zap.NewNop()))
	defer srv.Close()

	p := NewRemote(stubResolver{addr: srv.URL}, agent.NewClient(nil))
	s := &model.GameServer{ID: "srv-2", HostID: ptr("host-2"), Version: "1.20.4", CPUs: 1, MemoryMB: 1024}

	inst, err := p.Start(ctx, s)
	if err != nil {
		t.Fatalf("start (no vm): %v", err)
	}
	if inst.VMID == "" {
		t.Error("start without a recorded vm should provision a fresh one")
	}
}

func assertRemoteState(t *testing.T, p *RemoteProvisioner, s *model.GameServer, want State) {
	t.Helper()
	got, err := p.Status(context.Background(), s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}
