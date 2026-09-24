package service

import (
	"context"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// User is a users row as the surfaces see it.
type User = store.User

// Register creates a user and opens a session for them.
func (o *Orchestrator) Register(ctx context.Context, email, password, name string) (*auth.Session, error) {
	u, err := o.users.Register(ctx, email, password, name)
	if err != nil {
		return nil, err
	}
	return o.sessions.CreateSession(ctx, u.ID, nil)
}

// Login checks the credentials and opens a session.
func (o *Orchestrator) Login(ctx context.Context, email, password string) (*auth.Session, error) {
	u, err := o.users.Authenticate(ctx, email, password)
	if err != nil {
		return nil, err
	}
	return o.sessions.CreateSession(ctx, u.ID, nil)
}

// Logout closes the session behind a cookie token; an unknown token is a no-op.
func (o *Orchestrator) Logout(ctx context.Context, token string) error {
	s, err := o.sessions.ValidateSession(ctx, token)
	if err != nil || s == nil {
		return err
	}
	return o.sessions.DeleteSession(ctx, s.ID)
}

// User returns one user by id.
func (o *Orchestrator) User(ctx context.Context, id string) (User, error) {
	return o.users.Get(ctx, id)
}
