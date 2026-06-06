//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// registerHost registers a host via the agent endpoint and returns its record.
func registerHost(t *testing.T, hostname string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/register", "", map[string]any{
		"hostname":        hostname,
		"address":         "10.0.0.1:9000",
		"zone":            "zone-a",
		"cpus_total":      8,
		"memory_mb_total": 16384,
		"agent_version":   "0.1.0",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register host status = %d, body = %s", resp.StatusCode, body)
	}
	var h map[string]any
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatalf("decode host: %v (body=%s)", err, body)
	}
	return h
}

// adminHostByID returns the fleet host with the given id from the admin view,
// or nil if absent.
func adminHostByID(t *testing.T, adminToken, id string) map[string]any {
	t.Helper()
	resp, body := get(t, "/api/v1/admin/hosts", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list hosts status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Hosts []map[string]any `json:"hosts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	for _, h := range out.Hosts {
		if h["id"] == id {
			return h
		}
	}
	return nil
}

// waitForHostStatus polls the admin fleet view until host id reaches want.
func waitForHostStatus(t *testing.T, adminToken, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h := adminHostByID(t, adminToken, id); h != nil && h["status"] == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host %s did not reach status %q within timeout", id, want)
}

// TestHostFleetLifecycle exercises register -> heartbeat -> stale -> down ->
// recover through the agent endpoints and the admin fleet view.
func TestHostFleetLifecycle(t *testing.T) {
	admin := makeAdmin(t, "host-fleet-admin@example.com", "hunter2pass")

	host := registerHost(t, "host-lifecycle")
	id, _ := host["id"].(string)
	if id == "" {
		t.Fatalf("no id in register response: %v", host)
	}
	if host["status"] != "ready" {
		t.Errorf("status = %v, want ready", host["status"])
	}
	// Allocatable capacity is initialised to total on registration.
	if host["cpus_allocatable"] != host["cpus_total"] {
		t.Errorf("cpus_allocatable = %v, want %v", host["cpus_allocatable"], host["cpus_total"])
	}
	if host["memory_mb_allocatable"] != host["memory_mb_total"] {
		t.Errorf("memory_mb_allocatable = %v, want %v", host["memory_mb_allocatable"], host["memory_mb_total"])
	}

	// A heartbeat keeps the host alive.
	resp, body := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/"+id+"/heartbeat", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body = %s", resp.StatusCode, body)
	}

	// Stop heartbeating: the reaper marks the host down once its TTL lapses.
	waitForHostStatus(t, admin.AccessToken, id, "down")

	// A fresh heartbeat brings a downed host back to ready.
	resp, _ = doJSON(t, http.MethodPost, "/api/v1/agent/hosts/"+id+"/heartbeat", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovery heartbeat status = %d", resp.StatusCode)
	}
	if h := adminHostByID(t, admin.AccessToken, id); h == nil || h["status"] != "ready" {
		t.Fatalf("host did not recover to ready: %v", h)
	}
}

// TestHostReRegisterKeepsID verifies that re-registering the same hostname
// updates the existing record in place rather than creating a duplicate.
func TestHostReRegisterKeepsID(t *testing.T) {
	first := registerHost(t, "host-stable")
	second := registerHost(t, "host-stable")
	if first["id"] != second["id"] {
		t.Errorf("re-register changed id: %v -> %v", first["id"], second["id"])
	}
}

// TestRegisterWithAgentSuppliedID verifies that an agent-owned id is honored and
// is the authoritative key on re-registration — the basis for identity surviving
// a control-plane restart. A second register under the same id updates the
// existing record in place rather than minting a new one.
func TestRegisterWithAgentSuppliedID(t *testing.T) {
	const agentID = "11111111-1111-1111-1111-111111111111"

	resp, body := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/register", "", map[string]any{
		"id":              agentID,
		"hostname":        "host-owned-id",
		"address":         "10.0.0.5:9000",
		"cpus_total":      4,
		"memory_mb_total": 8192,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", resp.StatusCode, body)
	}
	var first map[string]any
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first["id"] != agentID {
		t.Fatalf("id = %v, want agent-supplied %s", first["id"], agentID)
	}

	// Re-register under the same id with changed capacity: same record, updated.
	resp, body = doJSON(t, http.MethodPost, "/api/v1/agent/hosts/register", "", map[string]any{
		"id":              agentID,
		"hostname":        "host-owned-id",
		"address":         "10.0.0.5:9000",
		"cpus_total":      16,
		"memory_mb_total": 32768,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-register status = %d, body = %s", resp.StatusCode, body)
	}
	var second map[string]any
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second["id"] != agentID {
		t.Errorf("re-register changed id: %v", second["id"])
	}
	if second["cpus_total"].(float64) != 16 {
		t.Errorf("cpus_total = %v, want 16 (updated in place)", second["cpus_total"])
	}
}

// TestRegisterRejectsBadID verifies a malformed agent id is rejected.
func TestRegisterRejectsBadID(t *testing.T) {
	resp, body := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/register", "", map[string]any{
		"id":              "not-a-uuid",
		"hostname":        "host-bad-id",
		"address":         "10.0.0.6:9000",
		"cpus_total":      4,
		"memory_mb_total": 8192,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// TestHeartbeatUnknownHost verifies an unknown host id is rejected with 404 so
// the agent knows to re-register.
func TestHeartbeatUnknownHost(t *testing.T) {
	resp, _ := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/00000000-0000-0000-0000-000000000000/heartbeat", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRegisterHostValidation covers request validation on the register endpoint.
func TestRegisterHostValidation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing hostname", map[string]any{"address": "10.0.0.1:9000", "cpus_total": 8, "memory_mb_total": 1024}},
		{"missing address", map[string]any{"hostname": "h", "cpus_total": 8, "memory_mb_total": 1024}},
		{"zero cpus", map[string]any{"hostname": "h", "address": "10.0.0.1:9000", "cpus_total": 0, "memory_mb_total": 1024}},
		{"zero memory", map[string]any{"hostname": "h", "address": "10.0.0.1:9000", "cpus_total": 8, "memory_mb_total": 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, http.MethodPost, "/api/v1/agent/hosts/register", "", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
		})
	}
}
