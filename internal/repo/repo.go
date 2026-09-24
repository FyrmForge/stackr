package repo

import (
	"context"

	"github.com/FyrmForge/hamr/pkg/auth"
)

// Store defines the data access interface for the application.
type Store interface {
	// Health checks the database connection.
	Health(ctx context.Context) error

	// Session persistence — satisfies auth.SessionStore.
	auth.SessionStore

	// GetUserByID returns a user by ID, or nil if not found.
	GetUserByID(ctx context.Context, id string) (*User, error)

	// GetUserByEmail returns a user by email, or nil if not found.
	GetUserByEmail(ctx context.Context, email string) (*User, error)

	// CreateUser inserts a new user.
	CreateUser(ctx context.Context, user *User) error
}
