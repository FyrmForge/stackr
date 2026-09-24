package service

import (
	"context"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	OrgMember      = store.OrgMember
	Invite         = store.Invite
	Credential     = store.Credential
	CredentialSpec = credential.Spec
	Connector      = store.Connector
)

// Orgs is what the user belongs to; AllOrgs is every org (admin).
func (o *Orchestrator) Orgs(ctx context.Context, userID string) ([]Org, error) {
	return o.orgs.ListForUser(ctx, userID)
}

func (o *Orchestrator) AllOrgs(ctx context.Context) ([]Org, error) { return o.orgs.ListAll(ctx) }

// CreateOrg returns the caller's draft org, made on first ask; RenameOrg and
// FinishOrg complete it.
func (o *Orchestrator) CreateOrg(ctx context.Context, userID string) (Org, error) {
	return o.orgs.StartDraft(ctx, userID)
}

// RenameOrg moves name and slug; the squat check reads every domain's org.
func (o *Orchestrator) RenameOrg(ctx context.Context, orgID, name string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	claims, err := o.claims(ctx)
	if err != nil {
		return og, err
	}
	return o.orgs.Rename(ctx, og, name, claims)
}

func (o *Orchestrator) FinishOrg(ctx context.Context, orgID string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	return o.orgs.SetupDone(ctx, og)
}

// DeleteOrg refuses while it has stacks, and the last org (leaf/org).
func (o *Orchestrator) DeleteOrg(ctx context.Context, orgID string) error {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return err
	}
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return err
	}
	return o.orgs.Delete(ctx, og, len(sts))
}

// claims is every domain host with its org.
func (o *Orchestrator) claims(ctx context.Context) ([]org.Claim, error) {
	ds, err := o.domains.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []org.Claim
	for _, d := range ds {
		t, err := o.tiles.Get(ctx, d.TileID)
		if err != nil {
			return nil, err
		}
		st, err := o.stacks.Get(ctx, t.StackID)
		if err != nil {
			return nil, err
		}
		out = append(out, org.Claim{Host: d.Host, OrgID: st.OrgID})
	}
	return out, nil
}

func (o *Orchestrator) Members(ctx context.Context, orgID string) ([]OrgMember, error) {
	return o.orgs.Members(ctx, orgID)
}

// SetRole changes a member's role; a drop below write closes their access
// in that org (B16).
func (o *Orchestrator) SetRole(ctx context.Context, orgID, userID, role string) error {
	from, err := o.orgs.SetRole(ctx, orgID, userID, role)
	if err != nil {
		return err
	}
	if closeIt, scope := authz.StandingChanged(orgID, from, role); closeIt {
		return o.users.CloseAccess(ctx, userID, scope)
	}
	return nil
}

// Invite makes a single-use invite link token (the invite id).
func (o *Orchestrator) Invite(ctx context.Context, orgID, email, role, by string) (Invite, error) {
	ms, err := o.orgs.Members(ctx, orgID)
	if err != nil {
		return Invite{}, err
	}
	member := false
	for _, m := range ms {
		if u, err := o.users.Get(ctx, m.UserID); err == nil && strings.EqualFold(u.Email, email) {
			member = true
		}
	}
	return o.orgs.Invite(ctx, orgID, email, role, by, member, time.Now())
}

func (o *Orchestrator) Invites(ctx context.Context, orgID string) ([]Invite, error) {
	return o.orgs.Invites(ctx, orgID)
}

// LookupInvite is the invite behind a link, refused when used or expired.
func (o *Orchestrator) LookupInvite(ctx context.Context, token string) (Invite, error) {
	return o.orgs.Lookup(ctx, token, time.Now())
}

// AcceptInvite joins an existing account: the burn and the join are one
// transaction (B14, B15).
func (o *Orchestrator) AcceptInvite(ctx context.Context, token, userID string) error {
	u, err := o.users.Get(ctx, userID)
	if err != nil {
		return err
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		_, err := org.New(tx.Orgs, tx.OrgMembers, tx.Invites).Accept(ctx, token, u.ID, u.Email, time.Now())
		return err
	})
}

// RegisterInvited creates the account and joins through the invite in one
// transaction, then opens a session.
func (o *Orchestrator) RegisterInvited(ctx context.Context, token, email, password, name string) (*auth.Session, error) {
	var id string
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		u, err := user.New(tx.Users, tx.Sessions, tx.APIKeys).Register(ctx, email, password, name)
		if err != nil {
			return err
		}
		id = u.ID
		_, err = org.New(tx.Orgs, tx.OrgMembers, tx.Invites).Accept(ctx, token, u.ID, u.Email, time.Now())
		return err
	})
	if err != nil {
		return nil, err
	}
	return o.sessions.CreateSession(ctx, id, nil)
}

// ---- registry credentials ----

func (o *Orchestrator) Credentials(ctx context.Context, orgID string) ([]Credential, error) {
	return o.creds.List(ctx, orgID)
}

func (o *Orchestrator) CreateCredential(ctx context.Context, orgID string, s CredentialSpec) (Credential, error) {
	return o.creds.Create(ctx, orgID, s)
}

func (o *Orchestrator) UpdateCredential(ctx context.Context, orgID, id string, s CredentialSpec) (Credential, error) {
	return o.creds.Update(ctx, orgID, id, s)
}

func (o *Orchestrator) DeleteCredential(ctx context.Context, orgID, id string) error {
	return o.creds.Delete(ctx, orgID, id)
}

// ---- git connectors ----

func (o *Orchestrator) Connectors(ctx context.Context, orgID string) ([]Connector, error) {
	return o.conns.List(ctx, orgID)
}

// BeginConnector makes the pending row; the browser POSTs manifest to action.
func (o *Orchestrator) BeginConnector(ctx context.Context, orgID, ghOrg string) (c Connector, action, manifest string, err error) {
	return o.conns.Begin(ctx, orgID, ghOrg)
}

// CompleteConnector takes GitHub's manifest callback.
func (o *Orchestrator) CompleteConnector(ctx context.Context, state, code string) (Connector, error) {
	return o.conns.Complete(ctx, state, code)
}

func (o *Orchestrator) RenameConnector(ctx context.Context, orgID, id, name string) (Connector, error) {
	return o.conns.Rename(ctx, orgID, id, name)
}

func (o *Orchestrator) DeleteConnector(ctx context.Context, orgID, id string) error {
	return o.conns.Delete(ctx, orgID, id)
}
