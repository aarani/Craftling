package repository

import (
	"context"
	"errors"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshTokenRepository provides persistence operations for refresh tokens.
type RefreshTokenRepository struct {
	pool *pgxpool.Pool
}

// NewRefreshTokenRepository constructs a RefreshTokenRepository.
func NewRefreshTokenRepository(pool *pgxpool.Pool) *RefreshTokenRepository {
	return &RefreshTokenRepository{pool: pool}
}

// Create persists a new refresh token (by hash) for the given user.
func (r *RefreshTokenRepository) Create(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	const q = `
		INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`
	_, err := r.pool.Exec(ctx, q, uuid.NewString(), userID, tokenHash, expiresAt)
	return err
}

// GetByHash returns the token with the given hash, or ErrNotFound.
func (r *RefreshTokenRepository) GetByHash(ctx context.Context, tokenHash string) (*model.RefreshToken, error) {
	const q = `
		SELECT id, user_id, token_hash, expires_at, revoked_at, created_at
		FROM refresh_tokens WHERE token_hash = $1`
	var t model.RefreshToken
	err := r.pool.QueryRow(ctx, q, tokenHash).
		Scan(&t.ID, &t.UserID, &t.TokenHash, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Revoke marks a single token as revoked (idempotent).
func (r *RefreshTokenRepository) Revoke(ctx context.Context, id string) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	_, err := r.pool.Exec(ctx, q, id)
	return err
}

// Rotate atomically revokes the old token and inserts a replacement, so a
// crash can never leave the old token revoked without a new one issued (or
// vice versa).
func (r *RefreshTokenRepository) Rotate(ctx context.Context, oldID, userID, newHash string, expiresAt time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit

	const revoke = `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`
	if _, err := tx.Exec(ctx, revoke, oldID); err != nil {
		return err
	}

	const insert = `INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, insert, uuid.NewString(), userID, newHash, expiresAt); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// DeleteExpired removes all tokens whose expiry has passed and reports how many
// rows were deleted. Revoked-but-unexpired tokens are kept so that reuse
// detection still works until they would have expired anyway.
func (r *RefreshTokenRepository) DeleteExpired(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RevokeAllForUser revokes every active token for a user. Used both for
// "log out everywhere" and as the response to detected token reuse.
func (r *RefreshTokenRepository) RevokeAllForUser(ctx context.Context, userID string) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	_, err := r.pool.Exec(ctx, q, userID)
	return err
}
