//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
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

// TestCapacityReconstructionAcrossRestart simulates a control-plane restart: the
// in-memory fleet is rebuilt from scratch, but a host re-registering with its
// stable id must regain the capacity already committed in the durable record,
// not come back as if empty. It exercises the real DB-backed UsedCapacity query
// plus the registration reconstruction the agent handler performs.
func TestCapacityReconstructionAcrossRestart(t *testing.T) {
	ctx := context.Background()
	const agentID = "33333333-3333-3333-3333-333333333333"

	// A server exists in the durable record, assigned to the host as if it were
	// placed before the restart. Create it through the API for a real row, then
	// pin its host_id to our agent id.
	user := registerUser(t, "restart-user@example.com", "hunter2pass")
	id := createServerID(t, user.AccessToken, "pre-restart-world")
	if _, err := pool.Exec(ctx, `UPDATE game_servers SET host_id = $2 WHERE id = $1`, id, agentID); err != nil {
		t.Fatalf("pin server to host: %v", err)
	}
	var specCPUs, specMemMB int
	if err := pool.QueryRow(ctx, `SELECT cpus, memory_mb FROM game_servers WHERE id = $1`, id).
		Scan(&specCPUs, &specMemMB); err != nil {
		t.Fatalf("read server spec: %v", err)
	}

	// Fresh control plane: a brand-new in-memory fleet. The host re-registers
	// with the same id; the registration path reads committed capacity from the
	// DB and seeds allocatable from it.
	freshHosts := repository.NewHostRepository()
	servers := repository.NewGameServerRepository(pool)
	usedCPUs, usedMemMB, err := servers.UsedCapacity(ctx, agentID)
	if err != nil {
		t.Fatalf("used capacity: %v", err)
	}
	if usedCPUs != specCPUs || usedMemMB != specMemMB {
		t.Fatalf("used = %d/%d, want %d/%d (the assigned server's spec)", usedCPUs, usedMemMB, specCPUs, specMemMB)
	}

	const totalCPUs, totalMemMB = 8, 8192
	h, err := freshHosts.RegisterReserved(ctx, &model.Host{
		ID: agentID, Hostname: "restart-host", Address: "10.0.0.200:9000",
		CPUsTotal: totalCPUs, MemoryMBTotal: totalMemMB,
	}, usedCPUs, usedMemMB)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.CPUsAllocatable != totalCPUs-specCPUs || h.MemoryMBAllocatable != totalMemMB-specMemMB {
		t.Fatalf("allocatable = %d/%d, want %d/%d (reconstructed, not reset to total)",
			h.CPUsAllocatable, h.MemoryMBAllocatable, totalCPUs-specCPUs, totalMemMB-specMemMB)
	}
}
