// Package org owns orgs, their members and invites: create (the one-draft
// setup), rename, delete, the last-owner guard and the invite rules. Facts
// from other tables (stack count, domain claims, "already a member") come in
// as arguments.
package org

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DraftName is the placeholder an org carries until setup names it.
const DraftName = "Untitled organization"

// The setup wizard's two branches (orgs.setup_mode): the org's config file
// names it, or its owner does, step by step.
const (
	SetupConfig = "config"
	SetupUI     = "ui"
)

// InviteTTL is how long an invite link works ("Auth mechanics": 7 days).
const InviteTTL = 7 * 24 * time.Hour

// Owner is the one role v1 writes (two roles: stackr admin and org owner).
const Owner = "owner"

// roles is every role the leaf writes, in the pickers' order.
// ponytail: owner only (DECIDE 171, darthvader 2026-09-25); member and
// viewer are Later. authz already ranks them.
var roles = []string{
	Owner,
}

// AssignableRoles is every role AddMember, SetRole and Invite take.
func AssignableRoles() []string { return slices.Clone(roles) }

func validRole(r string) bool { return slices.Contains(roles, r) }

type Leaf struct {
	orgs    store.OrgStore
	members store.OrgMemberStore
	invites store.InviteStore
}

// New takes the three tables. Build it on a store.Tx's tables when a caller
// needs Accept to be atomic with other writes (e.g. creating the account).
func New(orgs store.OrgStore, members store.OrgMemberStore, invites store.InviteStore) *Leaf {
	return &Leaf{orgs: orgs, members: members, invites: invites}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Org, error) { return l.orgs.Get(ctx, id) }

func (l *Leaf) GetBySlug(ctx context.Context, slug string) (store.Org, error) {
	return l.orgs.GetBySlug(ctx, slug)
}

// Resolve takes a slug first, an id second, so a slug that looks like an id
// is never shadowed.
func (l *Leaf) Resolve(ctx context.Context, ref string) (store.Org, error) {
	o, err := l.orgs.GetBySlug(ctx, ref)
	if errors.Is(err, errs.ErrNotFound) {
		return l.orgs.Get(ctx, ref)
	}
	return o, err
}

// ListAll crosses tenants: admin screens only. ListForUser is the scoped one.
func (l *Leaf) ListAll(ctx context.Context) ([]store.Org, error) { return l.orgs.List(ctx) }

func (l *Leaf) ListForUser(ctx context.Context, userID string) ([]store.Org, error) {
	ms, err := l.members.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]store.Org, 0, len(ms))
	for _, m := range ms {
		o, err := l.orgs.Get(ctx, m.OrgID)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Roles maps org id to the user's role there.
func (l *Leaf) Roles(ctx context.Context, userID string) (map[string]string, error) {
	ms, err := l.members.ListByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]string, len(ms))
	for _, m := range ms {
		roles[m.OrgID] = m.Role
	}
	return roles, nil
}

func (l *Leaf) Members(ctx context.Context, orgID string) ([]store.OrgMember, error) {
	return l.members.ListByOrg(ctx, orgID)
}

// StartDraft returns the caller's unfinished org, or creates one with the
// placeholder name and a random slug and makes the caller its owner. One
// draft per person: newest wins, and "owner" is this caller's own row, never
// "is an admin".
func (l *Leaf) StartDraft(ctx context.Context, userID string) (store.Org, error) {
	roles, err := l.Roles(ctx, userID)
	if err != nil {
		return store.Org{}, err
	}
	var draft *store.Org
	for id, role := range roles {
		if role != Owner {
			continue
		}
		o, err := l.orgs.Get(ctx, id)
		if err != nil {
			return store.Org{}, err
		}
		if o.SetupDoneAt == nil && (draft == nil || o.CreatedAt.After(draft.CreatedAt)) {
			draft = &o
		}
	}
	if draft != nil {
		return *draft, nil
	}
	now := time.Now().UTC()
	// Random, not counted: two people starting at once must not collide.
	o := store.Org{
		ID:        uuid.NewString(),
		Name:      DraftName,
		Slug:      "org-" + uuid.NewString()[:6],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}
	if err := l.orgs.Create(ctx, o); err != nil {
		return store.Org{}, err
	}
	return o, l.AddMember(ctx, o.ID, userID, Owner)
}

// Claim is one domain on the server and the org that holds it.
type Claim struct{ Host, OrgID string }

// Rename moves name and slug together. First refusal wins. claims is every
// domain on the server with its org, for the reverse squat check: renaming
// onto a slug some foreign domain leads with would hand this org that org's
// generated hostnames. No side effects beyond the row.
func (l *Leaf) Rename(ctx context.Context, o store.Org, name string, claims []Claim) (store.Org, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return o, errs.Invalidf("name", "Give the organization a name.")
	}
	s := slug.Make(name)
	if s == "" {
		return o, errs.Invalidf("name", "That name needs at least one letter or digit, since it becomes the URL.")
	}
	if holder, err := l.orgs.GetBySlug(ctx, s); err == nil && holder.ID != o.ID {
		return o, errs.Invalidf("name", "Another organization already uses that name.")
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return o, err
	}
	for _, c := range claims {
		if slug.OfHost(c.Host) == s && c.OrgID != o.ID {
			return o, errs.Invalidf("name", "Another organization's domain already leads with %q, so this name is not available.", s)
		}
	}
	o.Name, o.Slug = name, s
	return o, l.orgs.Update(ctx, o)
}

// SetupDone finishes setup; idempotent. Refused while the org still carries
// the placeholder name.
func (l *Leaf) SetupDone(ctx context.Context, o store.Org) (store.Org, error) {
	if o.SetupDoneAt != nil {
		return o, nil
	}
	if o.Name == DraftName {
		return o, errs.Conflictf("This organization has no name yet.")
	}
	now := time.Now().UTC()
	o.SetupDoneAt = &now
	return o, l.orgs.Update(ctx, o)
}

// SetSetupMode records the setup wizard's branch; anything else, or a
// finished org, is refused.
func (l *Leaf) SetSetupMode(ctx context.Context, o store.Org, mode string) (store.Org, error) {
	if mode != SetupConfig && mode != SetupUI {
		return o, errs.Invalidf("mode", "pick how to set the organization up")
	}
	if o.SetupDoneAt != nil {
		return o, errs.Conflictf("Setup is already finished.")
	}
	o.SetupMode = mode
	return o, l.orgs.Update(ctx, o)
}

// SetSettings stores the org's rung of the defaults cascade as given;
// leaf/settings owns its shape and the merge.
func (l *Leaf) SetSettings(ctx context.Context, o store.Org, blob string) (store.Org, error) {
	o.Settings = blob
	return o, l.orgs.Update(ctx, o)
}

// SetEnvColors stores the org's env colours: a JSON object of env slug to
// hue, kept canonical (sorted keys) so a diff compares it as text.
func (l *Leaf) SetEnvColors(ctx context.Context, o store.Org, colors string) (store.Org, error) {
	m := map[string]string{}
	if err := json.Unmarshal([]byte(colors), &m); err != nil {
		return o, errs.Invalidf("env_colors", "Env colours are a JSON object of env slug to colour.")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return o, err
	}
	o.EnvColors = string(b)
	return o, l.orgs.Update(ctx, o)
}

// SetConfigRepo binds the org to its config file: repo as githubapp.RepoURL
// spells it, "" unbinds and clears the other four columns too.
func (l *Leaf) SetConfigRepo(
	ctx context.Context,
	o store.Org,
	connectorID, repo, branch, path string,
	auto bool,
) (store.Org, error) {
	o.ConfigRepo = githubapp.RepoURL(repo)
	o.ConfigConnectorID = ""
	o.ConfigBranch = ""
	o.ConfigPath = ""
	o.ConfigAuto = false
	if o.ConfigRepo != "" {
		o.ConfigConnectorID = connectorID
		o.ConfigBranch = branch
		o.ConfigPath = path
		o.ConfigAuto = auto
	}
	return o, l.orgs.Update(ctx, o)
}

// Delete removes an org. stacks is the org's stack count. A draft is exempt
// from the last-org rule: on a fresh install it is the only org there is.
func (l *Leaf) Delete(ctx context.Context, o store.Org, stacks int) error {
	unfinished := o.SetupDoneAt == nil
	if stacks > 0 {
		if unfinished {
			return errs.Conflictf("This organization's config file already built stacks. Finish setup, then delete it from settings.")
		}
		return errs.Conflictf("Move or delete this org's stacks first.")
	}
	all, err := l.orgs.List(ctx)
	if err != nil {
		return err
	}
	if len(all) <= 1 && !unfinished {
		return errs.Conflictf("The last organization cannot be deleted.")
	}
	return l.orgs.Delete(ctx, o.ID)
}

// AddMember is idempotent: already a member keeps the role they have.
func (l *Leaf) AddMember(ctx context.Context, orgID, userID, role string) error {
	if !validRole(role) {
		return errs.Invalidf("role", "unknown role %q", role)
	}
	if _, err := l.members.GetByOrgUser(ctx, orgID, userID); err == nil {
		return nil
	} else if !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	return l.members.Create(ctx, store.OrgMember{
		ID:        uuid.NewString(),
		OrgID:     orgID,
		UserID:    userID,
		Role:      role,
		CreatedAt: time.Now().UTC(),
	})
}

// SetRole changes a member's role and returns the old one, for the revoke
// rule. A typo is refused, never folded; the last owner cannot be demoted.
func (l *Leaf) SetRole(ctx context.Context, orgID, userID, role string) (string, error) {
	if !validRole(role) {
		return "", errs.Invalidf("role", "unknown role %q", role)
	}
	m, err := l.members.GetByOrgUser(ctx, orgID, userID)
	if err != nil {
		return "", err
	}
	if role != Owner {
		if err := l.lastOwner(ctx, orgID, userID); err != nil {
			return "", err
		}
	}
	old := m.Role
	m.Role = role
	return old, l.members.Update(ctx, m)
}

// RemoveMember deletes the membership and returns the role it had, for the
// revoke rule. Not a member is ErrNotFound; the last owner stays.
func (l *Leaf) RemoveMember(ctx context.Context, orgID, userID string) (string, error) {
	m, err := l.members.GetByOrgUser(ctx, orgID, userID)
	if err != nil {
		return "", err
	}
	if err := l.lastOwner(ctx, orgID, userID); err != nil {
		return "", err
	}
	return m.Role, l.members.Delete(ctx, m.ID)
}

// lastOwner refuses to leave an org with nobody who can administer it. It
// fails closed on a read error.
func (l *Leaf) lastOwner(ctx context.Context, orgID, userID string) error {
	ms, err := l.members.ListByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ms, func(m store.OrgMember) bool { return m.Role == Owner && m.UserID != userID }) {
		return nil
	}
	return errs.Conflictf("an organization needs at least one owner")
}

// Invite mints an invite; the id is the link token (24 random bytes, hex).
// alreadyMember is whether the email belongs to a member of the org.
func (l *Leaf) Invite(
	ctx context.Context,
	orgID, email, role, by string,
	alreadyMember bool,
	now time.Time,
) (store.Invite, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return store.Invite{}, errs.Invalidf("email", "an email is required")
	}
	if role == "" {
		role = Owner
	}
	if !validRole(role) {
		return store.Invite{}, errs.Invalidf("role", "unknown role %q", role)
	}
	if alreadyMember {
		return store.Invite{}, errs.Conflictf("%s is already a member; change their role instead", email)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return store.Invite{}, err
	}
	i := store.Invite{
		ID:        hex.EncodeToString(b),
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		CreatedBy: by,
		CreatedAt: now,
		ExpiresAt: now.Add(InviteTTL),
	}
	return i, l.invites.Create(ctx, i)
}

// Invites lists an org's invites.
func (l *Leaf) Invites(ctx context.Context, orgID string) ([]store.Invite, error) {
	return l.invites.ListByOrg(ctx, orgID)
}

// Lookup reads an invite for the accept page: unknown, used and expired all
// read as ErrNotFound.
func (l *Leaf) Lookup(ctx context.Context, token string, now time.Time) (store.Invite, error) {
	i, err := l.invites.Get(ctx, token)
	if err != nil {
		return i, err
	}
	if !open(i, now) {
		return store.Invite{}, errs.ErrNotFound
	}
	return i, nil
}

// Pending is the org's invites still redeemable at now.
func (l *Leaf) Pending(ctx context.Context, orgID string, now time.Time) ([]store.Invite, error) {
	is, err := l.invites.ListByOrg(ctx, orgID)
	var out []store.Invite
	for _, i := range is {
		if open(i, now) {
			out = append(out, i)
		}
	}
	return out, err
}

func open(i store.Invite, now time.Time) bool { return i.UsedAt == nil && now.Before(i.ExpiresAt) }

// Accept burns the invite and adds the member (B14, B15). The burn is one
// conditional UPDATE, before the join: of two clicks exactly one wins. Run it
// inside store.Tx with a Leaf built on the Tx's tables, so a failed join
// un-burns and the account create (leaf/user) commits with it. email is the
// accepting account's; the link is not redeemable by anyone else.
func (l *Leaf) Accept(ctx context.Context, token, userID, email string, now time.Time) (store.Invite, error) {
	i, err := l.Lookup(ctx, token, now)
	if err != nil {
		return i, err
	}
	if !strings.EqualFold(strings.TrimSpace(email), i.Email) {
		return store.Invite{}, errs.ErrNotFound
	}
	if err := l.invites.Burn(ctx, i.ID, now); err != nil {
		return store.Invite{}, err
	}
	i.UsedAt = &now
	return i, l.AddMember(ctx, i.OrgID, userID, i.Role)
}
