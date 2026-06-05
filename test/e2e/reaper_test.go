//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/repository"
	"github.com/google/uuid"
)

// TestReapExpiredTokens verifies that DeleteExpired removes expired refresh
// tokens while leaving valid ones intact.
func TestReapExpiredTokens(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewRefreshTokenRepository(pool)

	// Create a user to satisfy the refresh_tokens foreign key.
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)`,
		userID, userID+"@example.com", "x",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	expiredHash := "expired-" + userID
	validHash := "valid-" + userID

	if err := repo.Create(ctx, userID, expiredHash, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	if err := repo.Create(ctx, userID, validHash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create valid token: %v", err)
	}

	n, err := repo.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 deleted, got %d", n)
	}

	if _, err := repo.GetByHash(ctx, expiredHash); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("expired token still present (err = %v)", err)
	}
	if _, err := repo.GetByHash(ctx, validHash); err != nil {
		t.Errorf("valid token was removed: %v", err)
	}
}
