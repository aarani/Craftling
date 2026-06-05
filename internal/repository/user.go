package repository

import (
	"context"
	"errors"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a user does not exist.
var ErrNotFound = errors.New("user not found")

// UserRepository provides persistence operations for users.
type UserRepository struct {
	pool *pgxpool.Pool
}

// NewUserRepository constructs a UserRepository backed by the given pool.
func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

// Create inserts a new user and returns it with its generated ID and timestamp.
func (r *UserRepository) Create(ctx context.Context, email, passwordHash string) (*model.User, error) {
	u := &model.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: passwordHash,
	}
	// role is assigned by the column default ('user') and read back.
	const q = `
		INSERT INTO users (id, email, password_hash)
		VALUES ($1, $2, $3)
		RETURNING role, created_at`
	if err := r.pool.QueryRow(ctx, q, u.ID, u.Email, u.PasswordHash).Scan(&u.Role, &u.CreatedAt); err != nil {
		return nil, err
	}
	return u, nil
}

// GetByEmail looks up a user by email, returning ErrNotFound when absent.
func (r *UserRepository) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	return r.getBy(ctx, `SELECT id, email, password_hash, role, created_at FROM users WHERE email = $1`, email)
}

// GetByID looks up a user by ID, returning ErrNotFound when absent.
func (r *UserRepository) GetByID(ctx context.Context, id string) (*model.User, error) {
	return r.getBy(ctx, `SELECT id, email, password_hash, role, created_at FROM users WHERE id = $1`, id)
}

// SetRole updates a user's role.
func (r *UserRepository) SetRole(ctx context.Context, id, role string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	return err
}

// List returns all users ordered by creation time (password hashes omitted).
func (r *UserRepository) List(ctx context.Context) ([]model.User, error) {
	const q = `SELECT id, email, role, created_at FROM users ORDER BY created_at`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []model.User
	for rows.Next() {
		var u model.User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (r *UserRepository) getBy(ctx context.Context, query string, arg any) (*model.User, error) {
	var u model.User
	err := r.pool.QueryRow(ctx, query, arg).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
