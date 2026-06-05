// Package provisioner abstracts the backend that actually runs game servers.
// The Fake implementation simulates provisioning; a real microVM backend
// (e.g. Firecracker / Cloud Hypervisor) implements the same interface.
package provisioner

import (
	"context"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
)

// Instance describes a provisioned server's runtime details.
type Instance struct {
	VMID string
	Host string
	Port int
}

// Provisioner creates and destroys the compute backing a game server.
type Provisioner interface {
	// Provision brings up the backing VM and returns its runtime details.
	Provision(ctx context.Context, s *model.GameServer) (*Instance, error)
	// Deprovision tears down the backing VM. It must be idempotent.
	Deprovision(ctx context.Context, s *model.GameServer) error
}

// defaultMinecraftPort is the standard Minecraft server port.
const defaultMinecraftPort = 25565

// Fake is an in-memory Provisioner that pretends to manage VMs. It lets the
// reconciler and API be exercised end-to-end before a real backend exists.
type Fake struct{}

// NewFake returns a Fake provisioner.
func NewFake() *Fake { return &Fake{} }

// Provision returns synthetic runtime details for the server.
func (Fake) Provision(_ context.Context, _ *model.GameServer) (*Instance, error) {
	return &Instance{
		VMID: "vm-" + uuid.NewString(),
		Host: "127.0.0.1",
		Port: defaultMinecraftPort,
	}, nil
}

// Deprovision is a no-op for the fake backend.
func (Fake) Deprovision(_ context.Context, _ *model.GameServer) error { return nil }
