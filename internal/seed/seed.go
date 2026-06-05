// Package seed bootstraps initial data such as the admin account.
package seed

import (
	"context"
	"errors"

	"github.com/aarani/craftling-go/internal/auth"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
)

// Admin ensures an admin user exists for the given credentials. If the user is
// absent it is created with the admin role; if it exists it is promoted to
// admin (its password is left untouched). It is a no-op when email or password
// is empty. The bool reports whether a new user was created.
func Admin(ctx context.Context, users *repository.UserRepository, email, password string) (bool, error) {
	if email == "" || password == "" {
		return false, nil
	}

	existing, err := users.GetByEmail(ctx, email)
	switch {
	case err == nil:
		if existing.Role != model.RoleAdmin {
			return false, users.SetRole(ctx, existing.ID, model.RoleAdmin)
		}
		return false, nil
	case !errors.Is(err, repository.ErrNotFound):
		return false, err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return false, err
	}
	u, err := users.Create(ctx, email, hash)
	if err != nil {
		return false, err
	}
	return true, users.SetRole(ctx, u.ID, model.RoleAdmin)
}
