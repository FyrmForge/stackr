package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/repo"
)

var (
	ErrEmailTaken        = errors.New("email already registered")
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

	now := time.Now()
	user := &repo.User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: hash,
		Name:         name,
		Role:         "user",
		Active:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.store.CreateUser(ctx, user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	return user, nil
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
	if !ok {
		return nil, ErrInvalidCredentials
	}

	return user, nil
}
