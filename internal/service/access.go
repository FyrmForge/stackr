package service

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Org         = store.Org
	Stack       = store.Stack
	Environment = store.Environment
	Tile        = store.Tile
)

// Principal is the request's user, loaded once per request from live rows.
type Principal struct {
	User   User
	Access authz.User
	KeyID  string // the API key behind the request, "" for a session
}

// SessionPrincipal loads the principal behind a browser session.
func (o *Orchestrator) SessionPrincipal(ctx context.Context, userID string) (*Principal, error) {
	u, err := o.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return o.principal(ctx, u, nil)
}

// KeyPrincipal loads the principal behind a bearer token. The key carries
// no rights of its own: the user's live role and memberships are read.
func (o *Orchestrator) KeyPrincipal(ctx context.Context, token string) (*Principal, error) {
	u, k, err := o.users.ByKey(ctx, token)
	if err != nil {
		return nil, err
	}
	p, err := o.principal(ctx, u, &k)
	if p != nil {
		p.KeyID = k.ID
	}
	return p, err
}

func (o *Orchestrator) principal(ctx context.Context, u User, k *store.APIKey) (*Principal, error) {
	roles, err := o.orgs.Roles(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	a := authz.User{ID: u.ID, Admin: u.Admin(), Active: u.Active, Roles: roles}
	if k != nil {
		a.Key = true
		if k.OrgID != nil {
			a.KeyOrg = *k.OrgID
		}
		if k.StackID != nil {
			a.KeyStack = *k.StackID
		}
		var ok bool
		a.KeyLevel, ok = authz.ParseLevel(k.Level)
		a.KeyCapped = ok
		if k.Level != "" && !ok {
			return nil, errs.ErrNotFound // an unknown ceiling fails closed, not as no ceiling
		}
	}
	return &Principal{User: u, Access: a}, nil
}

// Scope is what /:org/:stack/:env/:tile resolved to; nil past the last
// segment the route has.
type Scope struct {
	Org   *Org
	Stack *Stack
	Env   *Environment
	Tile  *Tile
	// Envs is the stack's environments (List's order) when an env
	// resolved: the top bar's env picker and every env's hue.
	Envs []Environment
}

// Resolve walks the slugs in order and stops at the first empty one; the
// first miss is errs.ErrNotFound.
func (o *Orchestrator) Resolve(ctx context.Context, org, stack, env, tile string) (Scope, error) {
	var s Scope
	if org == "" {
		return s, nil
	}
	og, err := o.orgs.GetBySlug(ctx, org)
	if errors.Is(err, errs.ErrNotFound) {
		// ponytail: an org answers to its id too (v0 did), for the setup
		// wizard's wait on an apply that renames the org under it; authz
		// still gates it. Drop it if an id-shaped slug ever matters.
		og, err = o.orgs.Get(ctx, org)
	}
	if err != nil {
		return s, err
	}
	s.Org = &og
	if stack == "" {
		return s, nil
	}
	st, err := o.stacks.GetBySlug(ctx, og.ID, stack)
	if err != nil {
		return s, err
	}
	s.Stack = &st
	if env == "" {
		return s, nil
	}
	en, err := o.envs.GetBySlug(ctx, st.ID, env)
	if err != nil {
		return s, err
	}
	s.Env = &en
	if s.Envs, err = o.envs.List(ctx, st.ID); err != nil {
		return s, err
	}
	if tile == "" {
		return s, nil
	}
	ti, err := o.tiles.GetBySlug(ctx, en.ID, tile)
	if err != nil {
		return s, err
	}
	s.Tile = &ti
	return s, nil
}

// DisableUser turns the account off and closes all its access (B16).
func (o *Orchestrator) DisableUser(ctx context.Context, userID string) error {
	return o.users.SetActive(ctx, userID, false)
}

// EnableUser turns the account back on. Only the flag returns: the sessions
// and keys DisableUser closed stay closed.
func (o *Orchestrator) EnableUser(ctx context.Context, userID string) error {
	return o.users.SetActive(ctx, userID, true)
}

// SetAdmin grants or takes the stackr admin role. Taking it closes the
// user's sessions and keys (B16).
func (o *Orchestrator) SetAdmin(ctx context.Context, userID string, admin bool) error {
	return o.users.SetAdmin(ctx, userID, admin)
}

// RemoveMember takes a user out of an org and applies the revoke rule.
func (o *Orchestrator) RemoveMember(ctx context.Context, orgID, userID string) error {
	role, err := o.orgs.RemoveMember(ctx, orgID, userID)
	if errors.Is(err, errs.ErrNotFound) {
		return errs.ErrNotFound
	}
	if err != nil {
		return err
	}
	if closeIt, scope := authz.StandingChanged(orgID, role, ""); closeIt {
		return o.users.CloseAccess(ctx, userID, scope)
	}
	return nil
}
