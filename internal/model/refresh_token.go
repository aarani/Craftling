package model

import "time"

// RefreshToken is a persisted, revocable token used to obtain new access tokens.
// Only the SHA-256 hash of the token value is stored.
type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// Revoked reports whether the token has been revoked.
func (t *RefreshToken) Revoked() bool { return t.RevokedAt != nil }

// Expired reports whether the token has expired as of now.
func (t *RefreshToken) Expired(now time.Time) bool { return now.After(t.ExpiresAt) }
