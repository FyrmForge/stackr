// Package user owns the users table, and closes a user's sessions and keys
// when their standing drops (B16).
package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct {
	users    store.UserStore
	sessions store.SessionStore
	keys     store.APIKeyStore
}

func New(users store.UserStore, sessions store.SessionStore, keys store.APIKeyStore) *Leaf {
	return &Leaf{users: users, sessions: sessions, keys: keys}
}

// ErrBadLogin is one answer for unknown email, wrong password and disabled
// account, so the form cannot be used to probe which emails exist.
var ErrBadLogin = errs.Invalidf("", "invalid email or password")

func (l *Leaf) Get(ctx context.Context, id string) (store.User, error) {
	return l.users.Get(ctx, id)
}

// Register creates an active, non-admin user.
func (l *Leaf) Register(ctx context.Context, email, password, name string) (store.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if _, err := l.users.GetByEmail(ctx, email); err == nil {
		return store.User{}, errs.Invalidf("email", "an account with this email already exists")
	} else if !errors.Is(err, errs.ErrNotFound) {
		return store.User{}, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return store.User{}, fmt.Errorf("hash password: %w", err)
	}
	now := time.Now().UTC()
	u := store.User{
		ID: uuid.NewString(), Email: email, PasswordHash: hash, Name: name,
		Role: "user", Active: true, Theme: "system", CreatedAt: now, UpdatedAt: now,
	}
	return u, l.users.Create(ctx, u)
}

// Authenticate checks the password of an active user.
func (l *Leaf) Authenticate(ctx context.Context, email, password string) (store.User, error) {
	u, err := l.users.GetByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, errs.ErrNotFound) {
		return store.User{}, ErrBadLogin
	}
	if err != nil {
		return store.User{}, err
	}
	ok, err := auth.CheckPassword(password, u.PasswordHash)
	if err != nil {
		return store.User{}, fmt.Errorf("check password: %w", err)
	}
	if !ok || !u.Active {
		return store.User{}, ErrBadLogin
	}
	return u, nil
}
