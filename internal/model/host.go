package model

import "time"

// Host statuses — the liveness/scheduling state of a worker host.
const (
	// HostReady means the host is healthy and eligible for placement.
	HostReady = "ready"
	// HostDraining means the host accepts no new placements and is shedding work.
	HostDraining = "draining"
	// HostDown means the host has missed heartbeats and is presumed unreachable.
	HostDown = "down"
)

// Host is a worker in the fleet that runs game-server microVMs. The control
// plane keeps an inventory of hosts and tracks their liveness via heartbeats;
// the scheduler (P2) places servers onto ready hosts with spare capacity.
//
// Total vs. allocatable: *_total is the host's physical capacity, while
// *_allocatable is what remains for new placements after current reservations.
type Host struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	Zone     string `json:"zone"`

	CPUsTotal           int `json:"cpus_total"`
	MemoryMBTotal       int `json:"memory_mb_total"`
	CPUsAllocatable     int `json:"cpus_allocatable"`
	MemoryMBAllocatable int `json:"memory_mb_allocatable"`

	Status       string `json:"status"`
	AgentVersion string `json:"agent_version"`

	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}
