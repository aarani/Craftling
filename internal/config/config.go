package config

import (
	"os"
	"strconv"
	"time"
)

// Process modes. The control plane runs as ModeServer; host workers will run
// as ModeAgent once the binary is split (P3). Today only the server exists.
const (
	ModeServer = "server"
	ModeAgent  = "agent"
)

// Config holds runtime configuration sourced from environment variables.
type Config struct {
	// Mode selects which role this process runs as: "server" (control plane)
	// or "agent" (host worker).
	Mode        string
	Port        string
	Env         string
	DatabaseURL string
	JWTSecret   string
	AccessTTL   time.Duration
	RefreshTTL  time.Duration

	// TemplateIndexURL is the registry/marketplace index the control plane fetches
	// the list of game-server templates from.
	TemplateIndexURL string

	// Optional admin bootstrap; when both are set, the admin is seeded on startup.
	AdminEmail    string
	AdminPassword string

	// Agent configuration (ModeAgent only). The host worker registers with the
	// control plane and exposes its VM API for the control plane to call back.
	Agent AgentConfig
}

// Agent runtime kinds: "fake" simulates VMs in memory (default, no KVM needed);
// "firecracker" boots real microVMs and requires /dev/kvm.
const (
	RuntimeFake        = "fake"
	RuntimeFirecracker = "firecracker"
)

// AgentConfig holds the host-worker settings used when Mode == ModeAgent.
type AgentConfig struct {
	// ControlPlaneURL is where the agent registers and heartbeats.
	ControlPlaneURL string
	// Runtime selects the VM backend: "fake" (default) or "firecracker".
	Runtime string
	// Firecracker holds the real-microVM driver settings (Runtime == "firecracker").
	Firecracker FirecrackerConfig
	// ID is this agent's stable, self-owned host id (kept across CP restarts).
	ID string
	// Hostname identifies the host in the fleet view.
	Hostname string
	// AdvertiseAddr is the agent's own API address the control plane calls back
	// (host:port reachable from the control plane).
	AdvertiseAddr string
	// AdvertiseHost is the player-facing connect address VMs report.
	AdvertiseHost string
	// Zone is an optional placement/locality label.
	Zone string
	// Version is reported to the control plane on register.
	Version string
	// CPUsTotal / MemoryMBTotal advertise this host's capacity to the scheduler.
	CPUsTotal     int
	MemoryMBTotal int
}

// FirecrackerConfig holds the paths the Firecracker driver needs (P4). It mirrors
// firecracker.Config; cmd/agent maps it across to avoid a config→driver import.
type FirecrackerConfig struct {
	// BinaryPath is the firecracker executable (empty: look up on PATH).
	BinaryPath string
	// KernelPath is the uncompressed kernel (vmlinux) all VMs boot.
	KernelPath string
	// ImageDir holds per-version base rootfs images (minecraft-<version>.ext4).
	ImageDir string
	// DefaultImage is the rootfs filename used when a version has no image.
	DefaultImage string
	// WorkDir is where per-VM working dirs live (empty: OS temp dir).
	WorkDir string
}

// Load reads configuration from the environment, applying sensible defaults.
func Load() *Config {
	return &Config{
		Mode:        getEnv("MODE", ModeServer),
		Port:        getEnv("PORT", "8080"),
		Env:         getEnv("APP_ENV", "development"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/craftling?sslmode=disable"),
		JWTSecret:   getEnv("JWT_SECRET", "dev-secret-change-me"),
		AccessTTL:   getDurationEnv("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:  getDurationEnv("REFRESH_TTL", 30*24*time.Hour),

		TemplateIndexURL: getEnv("TEMPLATE_INDEX_URL", "https://registry.craftling.io/manifest.json"),

		AdminEmail:    getEnv("ADMIN_EMAIL", ""),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),

		Agent: AgentConfig{
			ControlPlaneURL: getEnv("CONTROL_PLANE_URL", "http://localhost:8080"),
			Runtime:         getEnv("AGENT_RUNTIME", RuntimeFake),
			Firecracker: FirecrackerConfig{
				BinaryPath:   getEnv("FC_BINARY", ""),
				KernelPath:   getEnv("FC_KERNEL", ""),
				ImageDir:     getEnv("FC_IMAGE_DIR", ""),
				DefaultImage: getEnv("FC_DEFAULT_IMAGE", ""),
				WorkDir:      getEnv("FC_WORK_DIR", ""),
			},
			ID:            getEnv("AGENT_ID", ""),
			Hostname:      getEnv("AGENT_HOSTNAME", defaultHostname()),
			AdvertiseAddr: getEnv("ADVERTISE_ADDR", ""),
			AdvertiseHost: getEnv("ADVERTISE_HOST", "127.0.0.1"),
			Zone:          getEnv("ZONE", ""),
			Version:       getEnv("AGENT_VERSION", "0.1.0"),
			CPUsTotal:     getIntEnv("CPUS_TOTAL", 4),
			MemoryMBTotal: getIntEnv("MEMORY_MB_TOTAL", 8192),
		},
	}
}

// defaultHostname returns the OS hostname, or "agent" if it cannot be read.
func defaultHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "agent"
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
