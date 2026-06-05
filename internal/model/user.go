package model

import "time"

// Role constants. Single role per user.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User represents an application user.
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}
