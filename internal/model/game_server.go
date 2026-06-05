package model

import "time"

// Game kinds.
const (
	GameMinecraft = "minecraft"
)

// Desired states — what the user wants the server to be.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
	DesiredDeleted = "deleted"
)

// Actual statuses — where the reconciler has driven the server to.
const (
	StatusPending      = "pending"
	StatusProvisioning = "provisioning"
	StatusRunning      = "running"
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusDeleting     = "deleting"
	StatusDeleted      = "deleted"
	StatusError        = "error"
)

// GameServer is a user-owned game server backed (eventually) by a microVM.
// It separates desired state from observed status; a reconciler converges them.
type GameServer struct {
	ID       string `json:"id"`
	OwnerID  string `json:"owner_id"`
	Name     string `json:"name"`
	Game     string `json:"game"`
	Version  string `json:"version"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`

	DesiredState string `json:"desired_state"`
	Status       string `json:"status"`

	// Runtime details, populated once provisioned.
	VMID          *string `json:"vm_id,omitempty"`
	Host          *string `json:"host,omitempty"`
	Port          *int    `json:"port,omitempty"`
	StatusMessage *string `json:"status_message,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
