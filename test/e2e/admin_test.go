//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/seed"
)

// makeAdmin registers a user, promotes them to admin in the DB, and returns a
// freshly logged-in token pair carrying the admin role claim.
func makeAdmin(t *testing.T, email, password string) tokenPair {
	t.Helper()
	pair := registerUser(t, email, password)
	repo := repository.NewUserRepository(pool)
	if err := repo.SetRole(context.Background(), meID(t, pair.AccessToken), model.RoleAdmin); err != nil {
		t.Fatalf("set role: %v", err)
	}
	_, body := postJSON(t, "/api/v1/auth/login", map[string]string{
		"email": email, "password": password,
	})
	return tokensFrom(t, body)
}

// meID returns the authenticated user's id via /me.
func meID(t *testing.T, accessToken string) string {
	t.Helper()
	_, body := get(t, "/api/v1/me", accessToken)
	var u map[string]any
	if err := json.Unmarshal(body, &u); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	id, _ := u["id"].(string)
	if id == "" {
		t.Fatalf("no id in /me response: %s", body)
	}
	return id
}

// TestAdminAuthorization checks that the admin route is gated by role.
func TestAdminAuthorization(t *testing.T) {
	const adminRoute = "/api/v1/admin/users"

	t.Run("no token is unauthorized", func(t *testing.T) {
		resp, _ := get(t, adminRoute, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("non-admin is forbidden", func(t *testing.T) {
		user := registerUser(t, "plainuser@example.com", "hunter2pass")
		resp, _ := get(t, adminRoute, user.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("admin is allowed", func(t *testing.T) {
		admin := makeAdmin(t, "boss@example.com", "hunter2pass")
		resp, body := get(t, adminRoute, admin.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var out struct {
			Users []map[string]any `json:"users"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode users: %v", err)
		}
		if len(out.Users) == 0 {
			t.Error("expected at least one user in the list")
		}
	})
}

// TestAdminListServers verifies an admin sees servers across all owners, while
// a normal user cannot reach the endpoint.
func TestAdminListServers(t *testing.T) {
	// Two different owners each create a server.
	owner1 := registerUser(t, "fleet-owner1@example.com", "hunter2pass")
	owner2 := registerUser(t, "fleet-owner2@example.com", "hunter2pass")

	id1 := createServerID(t, owner1.AccessToken, "world-one")
	id2 := createServerID(t, owner2.AccessToken, "world-two")

	t.Run("non-admin is forbidden", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/admin/servers", owner1.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("admin sees every owner's servers", func(t *testing.T) {
		admin := makeAdmin(t, "fleet-admin@example.com", "hunter2pass")
		resp, body := get(t, "/api/v1/admin/servers", admin.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var out struct {
			Servers []map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode servers: %v", err)
		}
		seen := map[string]bool{}
		for _, s := range out.Servers {
			if sid, ok := s["id"].(string); ok {
				seen[sid] = true
			}
		}
		if !seen[id1] || !seen[id2] {
			t.Errorf("admin should see both servers; saw id1=%v id2=%v", seen[id1], seen[id2])
		}
	})
}

// TestSeedAdmin covers the admin bootstrap: creating a new admin and promoting
// an existing user, idempotently.
func TestSeedAdmin(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewUserRepository(pool)

	t.Run("creates a new admin", func(t *testing.T) {
		created, err := seed.Admin(ctx, repo, "seed-admin@example.com", "supersecret")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if !created {
			t.Fatal("expected created = true")
		}
		u, err := repo.GetByEmail(ctx, "seed-admin@example.com")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if u.Role != model.RoleAdmin {
			t.Errorf("role = %q, want admin", u.Role)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		created, err := seed.Admin(ctx, repo, "seed-admin@example.com", "supersecret")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if created {
			t.Error("expected created = false on second run")
		}
	})

	t.Run("promotes an existing user", func(t *testing.T) {
		if _, err := repo.Create(ctx, "promote-me@example.com", "x"); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if _, err := seed.Admin(ctx, repo, "promote-me@example.com", "ignored"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		u, err := repo.GetByEmail(ctx, "promote-me@example.com")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if u.Role != model.RoleAdmin {
			t.Errorf("role = %q, want admin", u.Role)
		}
	})

	t.Run("no-op without credentials", func(t *testing.T) {
		created, err := seed.Admin(ctx, repo, "", "")
		if err != nil || created {
			t.Fatalf("expected no-op, got created=%v err=%v", created, err)
		}
	})
}
