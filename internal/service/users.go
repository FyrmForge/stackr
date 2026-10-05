package service

import (
	"context"
	"path/filepath"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// User is a users row as the surfaces see it.
type User = store.User

// CreateAdmin makes the install's first account, the stackr admin. Only the
// installer reaches it (stackrd create-admin), beside a running panel, so it
// opens the database on its own: New would start a second job runner and
// scheduler. No route calls it.
func CreateAdmin(ctx context.Context, cfg Config, email, password, name string) (User, error) {
	var u User
	err := withUsers(ctx, cfg, func(l *user.Leaf) (err error) {
		u, err = l.CreateAdmin(ctx, email, password, name)
		return err
	})
	return u, err
}

// NeedsAdmin reports whether the install has no account yet, so a re-run of
// the installer knows to ask for one.
func NeedsAdmin(ctx context.Context, cfg Config) (bool, error) {
	var need bool
	err := withUsers(ctx, cfg, func(l *user.Leaf) error {
		all, err := l.List(ctx)
		need = len(all) == 0
		return err
	})
	return need, err
}

// withUsers opens the store alone, hands over the user leaf, and closes.
func withUsers(ctx context.Context, cfg Config, do func(*user.Leaf) error) error {
	box, err := secrets.New(cfg.SecretsKey)
	if err != nil {
		return err
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "stackr.db")
	}
	db, err := openDB(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	st := store.New(db, box)
	return do(user.New(st.Users, st.Sessions, st.APIKeys))
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
