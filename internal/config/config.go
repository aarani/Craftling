package config

import (
	"os"
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

	// Optional admin bootstrap; when both are set, the admin is seeded on startup.
	AdminEmail    string
	AdminPassword string
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

		AdminEmail:    getEnv("ADMIN_EMAIL", ""),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
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
