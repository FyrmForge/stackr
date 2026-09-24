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
}

// SessionPrincipal loads the principal behind a browser session.
func (o *Orchestrator) SessionPrincipal(ctx context.Context, userID string) (*Principal, error) {
	u, err := o.users.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return o.principal(ctx, u, false, "")
}

// KeyPrincipal loads the principal behind a bearer token. The key carries
// no rights of its own: the user's live role and memberships are read.
func (o *Orchestrator) KeyPrincipal(ctx context.Context, token string) (*Principal, error) {
	u, k, err := o.users.ByKey(ctx, token)
	if err != nil {
		return nil, err
	}
	org := ""
	if k.OrgID != nil {
		org = *k.OrgID
	}
	return o.principal(ctx, u, true, org)
}

func (o *Orchestrator) principal(ctx context.Context, u User, key bool, keyOrg string) (*Principal, error) {
	roles, err := o.orgs.Roles(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return &Principal{User: u, Access: authz.User{
		ID: u.ID, Admin: u.Role == "admin", Active: u.Active,
		Roles: roles, Key: key, KeyOrg: keyOrg,
	}}, nil
}

// Scope is what /:org/:stack/:env/:tile resolved to; nil past the last
// segment the route has.
type Scope struct {
	Org   *Org
	Stack *Stack
	Env   *Environment
	Tile  *Tile
}

// Resolve walks the slugs in order and stops at the first empty one; the
// first miss is errs.ErrNotFound.
func (o *Orchestrator) Resolve(ctx context.Context, org, stack, env, tile string) (Scope, error) {
	var s Scope
	if org == "" {
		return s, nil
	}
	og, err := o.orgs.GetBySlug(ctx, org)
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
	if err := o.users.SetActive(ctx, userID, false); err != nil {
		return err
	}
	return o.users.CloseAccess(ctx, userID, "")
}

// SetAdmin grants or takes the stackr admin role. Taking it closes the
// user's sessions and keys (B16).
func (o *Orchestrator) SetAdmin(ctx context.Context, userID string, admin bool) error {
	if err := o.users.SetAdmin(ctx, userID, admin); err != nil || admin {
		return err
	}
	return o.users.CloseAccess(ctx, userID, "")
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
