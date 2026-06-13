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
	// StatusUnschedulable means the server wants to run but no host currently has
	// the spare capacity to place it. The reconciler retries on each tick.
	StatusUnschedulable = "unschedulable"
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

	// Image and ImageDigest pin the OCI/docker image the VM boots from.
	// When Image is set the agent builds a squashfs rootfs from it (see
	// internal/image) instead of the legacy per-version ext4 base; the
	// control plane resolves the digest at create time so the rootfs is
	// reproducible. Empty Image keeps the legacy path.
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`

	// Env is the per-server environment as "KEY=VALUE" entries, resolved from a
	// marketplace template's answers at create time. The agent merges these over
	// the image's own OCI env (these win on conflict) and delivers the result to
	// the guest init via MMDS. Empty keeps the image's stock environment.
	Env []string `json:"env,omitempty"`

	DesiredState string `json:"desired_state"`
	Status       string `json:"status"`

	// HostID is the fleet host the scheduler placed this server on (P2). It is
	// set before provisioning and persists across stop/start (the VM stays put);
	// it is cleared only on delete. Nil until placed.
	HostID *string `json:"host_id,omitempty"`

	// Runtime details, populated once provisioned.
	VMID          *string `json:"vm_id,omitempty"`
	Host          *string `json:"host,omitempty"`
	Port          *int    `json:"port,omitempty"`
	StatusMessage *string `json:"status_message,omitempty"`

	// BackupRequested is a user-set flag asking for an on-demand world snapshot
	// (P5). The reconciler — the sole writer of compute side effects — performs
	// the snapshot via the agent, then clears the flag and stamps LastBackupAt.
	BackupRequested bool       `json:"backup_requested"`
	LastBackupAt    *time.Time `json:"last_backup_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
