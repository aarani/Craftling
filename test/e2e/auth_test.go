//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// tokenPair mirrors the auth endpoints' JSON response.
type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// postJSON issues a POST with a JSON body and returns the response and raw body.
func postJSON(t *testing.T, path string, body any) (*http.Response, []byte) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp, readBody(t, resp)
}

// get issues a GET, optionally with a Bearer token.
func get(t *testing.T, path, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp, readBody(t, resp)
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func tokensFrom(t *testing.T, body []byte) tokenPair {
	t.Helper()
	var tp tokenPair
	if err := json.Unmarshal(body, &tp); err != nil {
		t.Fatalf("decode tokens: %v (body=%s)", err, body)
	}
	if tp.AccessToken == "" || tp.RefreshToken == "" {
		t.Fatalf("expected access and refresh tokens, body=%s", body)
	}
	return tp
}

// registerUser is a helper that registers a fresh user and returns the token pair.
func registerUser(t *testing.T, email, password string) tokenPair {
	t.Helper()
	resp, body := postJSON(t, "/api/v1/auth/register", map[string]string{
		"email": email, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", resp.StatusCode, body)
	}
	return tokensFrom(t, body)
}

// TestAuthFlow walks the full register -> login -> access-protected-route path.
func TestAuthFlow(t *testing.T) {
	const (
		email    = "alice@example.com"
		password = "hunter2pass"
	)
	pair := registerUser(t, email, password)

	t.Run("register returns a bearer pair", func(t *testing.T) {
		if pair.TokenType != "Bearer" {
			t.Errorf("token_type = %q, want Bearer", pair.TokenType)
		}
		if pair.ExpiresIn <= 0 {
			t.Errorf("expires_in = %d, want > 0", pair.ExpiresIn)
		}
	})

	t.Run("duplicate register conflicts", func(t *testing.T) {
		resp, body := postJSON(t, "/api/v1/auth/register", map[string]string{
			"email": email, "password": password,
		})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
	})

	t.Run("login with wrong password is unauthorized", func(t *testing.T) {
		resp, body := postJSON(t, "/api/v1/auth/login", map[string]string{
			"email": email, "password": "wrongpassword",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
	})

	t.Run("login with correct password returns a pair", func(t *testing.T) {
		resp, body := postJSON(t, "/api/v1/auth/login", map[string]string{
			"email": email, "password": password,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		tokensFrom(t, body)
	})

	t.Run("me without token is unauthorized", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/me", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("me with valid access token returns the user", func(t *testing.T) {
		resp, body := get(t, "/api/v1/me", pair.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		var user map[string]any
		if err := json.Unmarshal(body, &user); err != nil {
			t.Fatalf("decode user: %v (body=%s)", err, body)
		}
		if user["email"] != email {
			t.Errorf("email = %v, want %s", user["email"], email)
		}
		if user["role"] != "user" {
			t.Errorf("role = %v, want user", user["role"])
		}
		if _, leaked := user["password_hash"]; leaked {
			t.Error("password_hash leaked in /me response")
		}
	})
}

// TestRefreshRotation covers token rotation and reuse detection.
func TestRefreshRotation(t *testing.T) {
	pair := registerUser(t, "refresh@example.com", "hunter2pass")

	// Exchange the refresh token for a brand-new pair.
	resp, body := postJSON(t, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": pair.RefreshToken,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", resp.StatusCode, body)
	}
	rotated := tokensFrom(t, body)

	if rotated.RefreshToken == pair.RefreshToken {
		t.Error("expected a rotated (different) refresh token")
	}

	t.Run("new access token works", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/me", rotated.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("new refresh token works", func(t *testing.T) {
		resp, _ := postJSON(t, "/api/v1/auth/refresh", map[string]string{
			"refresh_token": rotated.RefreshToken,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("reusing the original (rotated-away) token is rejected", func(t *testing.T) {
		resp, _ := postJSON(t, "/api/v1/auth/refresh", map[string]string{
			"refresh_token": pair.RefreshToken,
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("reuse detection revokes the whole family", func(t *testing.T) {
		// After the reuse above, even the most recently rotated token is dead.
		resp, _ := postJSON(t, "/api/v1/auth/refresh", map[string]string{
			"refresh_token": rotated.RefreshToken,
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestLogout verifies a logged-out refresh token can no longer be used.
func TestLogout(t *testing.T) {
	pair := registerUser(t, "logout@example.com", "hunter2pass")

	resp, _ := postJSON(t, "/api/v1/auth/logout", map[string]string{
		"refresh_token": pair.RefreshToken,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}

	resp, _ = postJSON(t, "/api/v1/auth/refresh", map[string]string{
		"refresh_token": pair.RefreshToken,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after logout = %d, want 401", resp.StatusCode)
	}
}

// TestRefreshInvalid covers malformed/unknown refresh requests.
func TestRefreshInvalid(t *testing.T) {
	t.Run("unknown token", func(t *testing.T) {
		resp, _ := postJSON(t, "/api/v1/auth/refresh", map[string]string{
			"refresh_token": "this-is-not-a-real-token",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		resp, _ := postJSON(t, "/api/v1/auth/refresh", map[string]string{})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// TestRegisterValidation covers request-body validation failures.
func TestRegisterValidation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]string
	}{
		{"short password", map[string]string{"email": "bob@example.com", "password": "short"}},
		{"invalid email", map[string]string{"email": "not-an-email", "password": "longenough1"}},
		{"missing password", map[string]string{"email": "carol@example.com"}},
		{"missing email", map[string]string{"password": "longenough1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postJSON(t, "/api/v1/auth/register", tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
		})
	}
}
