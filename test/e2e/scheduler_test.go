//go:build e2e

package e2e

import (
	"net/http"
	"testing"
)

// TestServerPlacement verifies the scheduler places a created server onto a
// fleet host: it reaches running with a host_id set, and that host's allocatable
// capacity reflects the reservation.
func TestServerPlacement(t *testing.T) {
	admin := makeAdmin(t, "placement-admin@example.com", "hunter2pass")
	user := registerUser(t, "placement-user@example.com", "hunter2pass")

	id := createServerID(t, user.AccessToken, "placed-world")
	running := waitForStatus(t, user.AccessToken, id, "running")

	hostID, _ := running["host_id"].(string)
	if hostID == "" {
		t.Fatalf("running server has no host_id: %v", running)
	}

	// The host it landed on must show capacity carved out by the reservation.
	h := adminHostByID(t, admin.AccessToken, hostID)
	if h == nil {
		t.Fatalf("placement host %s not present in admin fleet view", hostID)
	}
	if h["cpus_allocatable"].(float64) >= h["cpus_total"].(float64) {
		t.Errorf("cpus_allocatable %v not reduced below total %v after placement",
			h["cpus_allocatable"], h["cpus_total"])
	}
	if h["memory_mb_allocatable"].(float64) >= h["memory_mb_total"].(float64) {
		t.Errorf("memory_mb_allocatable %v not reduced below total %v after placement",
			h["memory_mb_allocatable"], h["memory_mb_total"])
	}
}

// TestOversizeServerRejected verifies create-time validation rejects a spec no
// host in the fleet could ever run. The placement host's memory total is below
// the maximum allowed memory_mb, so a max-memory request exceeds every host.
func TestOversizeServerRejected(t *testing.T) {
	user := registerUser(t, "oversize-user@example.com", "hunter2pass")

	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", user.AccessToken, map[string]any{
		"name":      "too-big",
		"version":   "1.20.4",
		"memory_mb": placementHostMemoryMB * 2,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400 (exceeds fleet capacity)", resp.StatusCode, body)
	}
}
