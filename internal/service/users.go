package service

import (
	"context"
	"path/filepath"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// User is a users row as the surfaces see it.
type User = store.User

// CreateAdmin makes the install's first account, the stackr admin, and its
// first API key, bound to no org, for the CLI on the controller (server admin
// commands refuse an org's key). Only the installer reaches it (stackrd
// create-admin), beside a running panel, so it opens the database on its
// own: New would start a second job runner and scheduler. No route calls it.
func CreateAdmin(ctx context.Context, cfg Config, email, password, name string) (User, string, error) {
	var u User
	var tok string
	err := withStore(ctx, cfg, func(st *store.Store) error {
		return st.Tx(ctx, func(tx store.Tx) (err error) {
			l := user.New(tx.Users, tx.Sessions, tx.APIKeys)
			if u, err = l.CreateAdmin(ctx, email, password, name); err != nil {
				return err
			}
			var k store.APIKey
			if tok, k, err = l.MintKey(ctx, u, "", "", "installer", "", ""); err != nil {
				return err
			}
			return audit(tx).Append(ctx, securitylog.Event{
				Action: securitylog.KeyMint, ActorID: u.ID, TargetKind: "api_key", TargetID: k.ID,
				Detail: "name=installer org= level= stack=",
			})
		})
	})
	return u, tok, err
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
	return withStore(ctx, cfg, func(st *store.Store) error { return do(user.New(st.Users, st.Sessions, st.APIKeys)) })
}

// withStore opens the store alone, hands it over, and closes.
func withStore(ctx context.Context, cfg Config, do func(*store.Store) error) error {
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
	return do(store.New(db, box))
}

// Login checks the credentials and opens a session. Either way one security
// event is written; a success shares its transaction with the session.
func (o *Orchestrator) Login(ctx context.Context, email, password string, meta RequestMeta) (*auth.Session, error) {
	u, err := o.users.Authenticate(ctx, email, password)
	if err != nil {
		if _, bad := errs.IsInvalid(err); bad {
			if aerr := o.auditLogin(ctx, securitylog.LoginBad, email, meta); aerr != nil {
				return nil, aerr
			}
		}
		return nil, err
	}
	var s *auth.Session
	err = o.store.Tx(ctx, func(tx store.Tx) (err error) {
		if s, err = o.openSession(ctx, tx, u.ID); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.LoginOK, ActorID: u.ID, TargetKind: "user", TargetID: u.ID,
			IP: meta.IP, UserAgent: meta.UserAgent,
		})
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Logout closes the session behind a cookie token; an unknown token is a no-op.
func (o *Orchestrator) Logout(ctx context.Context, token string) error {
	s, err := o.sessions.ValidateSession(ctx, token)
	if err != nil || s == nil {
		return err
	}
	return o.sessions.DeleteSession(ctx, s.ID)
}
