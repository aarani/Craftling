package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// GenerateRefreshToken returns a cryptographically random opaque token along
// with the hash that should be persisted. The raw token is given to the client
// and never stored; only the hash is kept server-side.
func GenerateRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken returns the hex-encoded SHA-256 hash of a refresh token.
// SHA-256 (not bcrypt) is appropriate here because the input is already
// high-entropy random data, so it needs no slow, salted KDF.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
