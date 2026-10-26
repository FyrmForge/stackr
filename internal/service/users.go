package service

import (
	"context"
	"path/filepath"
	"slices"

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

// MintAdminKey mints a new API key, bound to no org, for the oldest active
// stackr admin: the restore's way to log the controller's CLI back in, since
// the old key went with the old box's cli.json. Like CreateAdmin, only the
// installer reaches it (stackrd admin-key). org is the slug the CLI starts
// on: the admin's oldest membership, else the oldest org, "" with none.
func MintAdminKey(ctx context.Context, cfg Config) (u User, tok, org string, err error) {
	err = withStore(ctx, cfg, func(st *store.Store) error {
		return st.Tx(ctx, func(tx store.Tx) error {
			l := user.New(tx.Users, tx.Sessions, tx.APIKeys)
			all, err := l.List(ctx)
			if err != nil {
				return err
			}
			var found bool
			for _, x := range all {
				if x.Admin() && x.Active && (!found || x.CreatedAt.Before(u.CreatedAt)) {
					u, found = x, true
				}
			}
			if !found {
				return errs.Conflictf("no active stackr admin")
			}
			var k store.APIKey
			if tok, k, err = l.MintKey(ctx, u, "", "", "restore", "", ""); err != nil {
				return err
			}
			if err := audit(tx).Append(ctx, securitylog.Event{
				Action: securitylog.KeyMint, ActorID: u.ID, TargetKind: "api_key", TargetID: k.ID,
				Detail: "name=restore org= level= stack=",
			}); err != nil {
				return err
			}
			if org, err = firstOrg(ctx, tx, u.ID); err != nil {
				return err
			}
			return revokeBoxKeys(ctx, tx, l, u.ID, k.ID)
		})
	})
	return u, tok, org, err
}

// firstOrg is the slug of the user's oldest membership, else of the oldest
// org; "" when there is no org.
func firstOrg(ctx context.Context, tx store.Tx, userID string) (string, error) {
	ms, err := tx.OrgMembers.ListByUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(ms) > 0 {
		m := slices.MinFunc(ms, func(a, b store.OrgMember) int { return a.CreatedAt.Compare(b.CreatedAt) })
		og, err := tx.Orgs.Get(ctx, m.OrgID)
		return og.Slug, err
	}
	all, err := tx.Orgs.List(ctx)
	if err != nil || len(all) == 0 {
		return "", err
	}
	return slices.MinFunc(all, func(a, b store.Org) int { return a.CreatedAt.Compare(b.CreatedAt) }).Slug, nil
}

// revokeBoxKeys revokes the user's earlier box CLI keys (named installer or
// restore) but keep: the new cli.json holds keep, and the old ones lived on a
// box that is gone or in an archive.
func revokeBoxKeys(ctx context.Context, tx store.Tx, l *user.Leaf, userID, keep string) error {
	ks, err := l.Keys(ctx, userID)
	if err != nil {
		return err
	}
	for _, k := range ks {
		if k.ID == keep || (k.Name != "installer" && k.Name != "restore") {
			continue
		}
		if err := l.RevokeKey(ctx, userID, k.ID); err != nil {
			return err
		}
		if err := audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.KeyRevoke, ActorID: userID, TargetKind: "api_key", TargetID: k.ID,
			Detail: "name=" + k.Name + ": replaced by the box CLI key " + keep,
		}); err != nil {
			return err
		}
	}
	return nil
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
