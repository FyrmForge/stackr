// Package service holds business logic that outlives any one HTTP handler.
// Today that is authentication: registration, login and the password rules,
// kept here so the web and API layers apply the same ones.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid credentials")
)

// AuthService handles authentication logic.
type AuthService struct {
	store repo.Store
}

// NewAuthService creates a new auth service.
func NewAuthService(store repo.Store) *AuthService {
	return &AuthService{store: store}
}

// Register creates a new user with a hashed password.
func (s *AuthService) Register(ctx context.Context, email, password, name string) (*repo.User, error) {
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("check existing user: %w", err)
	}
	if existing != nil {
		return nil, ErrEmailTaken
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	// The first account on a fresh install is the server admin.
	role := "user"
	if n, err := s.store.CountUsers(ctx); err == nil && n == 0 {
		role = "admin"
	}

	now := time.Now()
	user := &repo.User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: hash,
		Name:         name,
		Role:         role,
		Active:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.store.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	return user, nil
}

// ChangePassword swaps a user's password after verifying the current one.
// The current-password check is what stops a stolen session from locking the
// real owner out of their own account.
func (s *AuthService) ChangePassword(ctx context.Context, userID, current, next string) error {
	user, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("find user: %w", err)
	}
	if user == nil {
		return ErrInvalidCredentials
	}
	ok, err := auth.CheckPassword(current, user.PasswordHash)
	if err != nil {
		return fmt.Errorf("check password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	user.PasswordHash = hash
	user.UpdatedAt = time.Now()
	if err := s.store.UpdateUser(ctx, user); err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

// Authenticate verifies credentials and returns the user.
func (s *AuthService) Authenticate(ctx context.Context, email, password string) (*repo.User, error) {
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user == nil {
		return nil, ErrInvalidCredentials
	}

	ok, err := auth.CheckPassword(password, user.PasswordHash)
	if err != nil {
		return nil, fmt.Errorf("check password: %w", err)
	}
	if !ok || !user.Active {
		return nil, ErrInvalidCredentials
	}

	return user, nil
}
