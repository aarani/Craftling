//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAgentSeam verifies the control plane drives the VM on the host agent
// across the network seam (P3): a created server's VM actually exists and runs
// on the in-process agent, and deleting the server tears that VM down.
func TestAgentSeam(t *testing.T) {
	user := registerUser(t, "seam-user@example.com", "hunter2pass")
	tok := user.AccessToken

	id := createServerID(t, tok, "seam-world")
	running := waitForStatus(t, tok, id, "running")

	vmID, _ := running["vm_id"].(string)
	if vmID == "" {
		t.Fatalf("running server has no vm_id: %v", running)
	}

	// The agent must report this VM as running, and tagged with the server id.
	vm := agentVM(t, vmID)
	if vm["state"] != "running" {
		t.Errorf("agent vm state = %v, want running", vm["state"])
	}
	if vm["server_id"] != id {
		t.Errorf("agent vm server_id = %v, want %s", vm["server_id"], id)
	}

	// Deleting the server deprovisions the VM on the agent.
	resp, _ := doJSON(t, http.MethodDelete, "/api/v1/servers/"+id, tok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", resp.StatusCode)
	}
	waitForGone(t, tok, id)

	if vm := agentVM(t, vmID); vm["state"] != "missing" {
		t.Errorf("after delete, agent vm state = %v, want missing", vm["state"])
	}
}

// agentVM fetches a VM's record directly from the in-process agent API.
func agentVM(t *testing.T, vmID string) map[string]any {
	t.Helper()
	resp, err := http.Get(agentBaseURL + "/vms/" + vmID)
	if err != nil {
		t.Fatalf("get agent vm: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent vm status = %d", resp.StatusCode)
	}
	var vm map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&vm); err != nil {
		t.Fatalf("decode agent vm: %v", err)
	}
	return vm
}
