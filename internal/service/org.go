package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
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
	Repo           = githubapp.Repo
)

// DraftOrgName is what an org is called until setup names it; SetupConfig
// and SetupUI are the setup wizard's branches.
const (
	DraftOrgName = org.DraftName
	SetupConfig  = org.SetupConfig
	SetupUI      = org.SetupUI
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

// SetOrgSetupMode picks the setup wizard's branch. By hand drops the
// config binding, and the plan waiting on it is rejected (v0 SetupMode).
func (o *Orchestrator) SetOrgSetupMode(ctx context.Context, orgID, mode string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	if og, err = o.orgs.SetSetupMode(ctx, og, mode); err != nil || mode != SetupUI {
		return og, err
	}
	return o.SetOrgConfigRepo(ctx, og.ID, "", "", "", "", false)
}

// RenameOrg moves name and slug, and the auto domains under the org with
// them; the squat check reads every domain's org.
func (o *Orchestrator) RenameOrg(ctx context.Context, orgID, name string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	claims, err := o.claims(ctx)
	if err != nil {
		return og, err
	}
	if og, err = o.orgs.Rename(ctx, og, name, claims); err != nil {
		return og, err
	}
	ts, err := o.scopeTiles(ctx, ParamScope{Kind: "org", ID: og.ID})
	if err != nil {
		return og, err
	}
	return og, o.refreshAutoHosts(ctx, ts)
}

// SetOrgSettings writes the org's rung of the defaults cascade and
// redeploys the running tiles of every stack in the org (B34).
func (o *Orchestrator) SetOrgSettings(ctx context.Context, orgID, blob string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	if og, err = o.orgs.SetSettings(ctx, og, blob); err != nil {
		return og, err
	}
	return og, o.redeployScope(ctx, ParamScope{Kind: "org", ID: orgID})
}

// SetOrgEnvColors writes the org's env colours (a JSON object of env slug
// to colour) and redeploys the org's running tiles, as SetOrgSettings does.
// ponytail: a colour never reaches a container, so the redeploy restarts
// for nothing; the step 7 task asks for it. Drop it once that is settled.
func (o *Orchestrator) SetOrgEnvColors(ctx context.Context, orgID, colors string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	if og, err = o.orgs.SetEnvColors(ctx, og, colors); err != nil {
		return og, err
	}
	return og, o.redeployScope(ctx, ParamScope{Kind: "org", ID: orgID})
}

// FinishOrg completes setup, and gives an org with no domain resource of its
// own the undeclared <slug>.<instance host> (v0's ensureDefaultDomain).
func (o *Orchestrator) FinishOrg(ctx context.Context, orgID string) (Org, error) {
	og, err := o.orgs.Get(ctx, orgID)
	if err != nil {
		return og, err
	}
	if og, err = o.orgs.SetupDone(ctx, og); err != nil {
		return og, err
	}
	return og, o.domainres.EnsureOrg(ctx, og.ID, og.Slug)
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

// claims is every host with its org: tile domains, and org and stack domain
// resources.
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
	rs, err := o.domainres.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		switch {
		case r.OrgID != nil:
			out = append(out, org.Claim{Host: r.Host, OrgID: *r.OrgID})
		case r.StackID != nil:
			st, err := o.stacks.Get(ctx, *r.StackID)
			if err != nil {
				return nil, err
			}
			out = append(out, org.Claim{Host: r.Host, OrgID: st.OrgID})
		}
	}
	return out, nil
}

// Roles is every role a member or an invite can hold, in the pickers'
// order.
func (o *Orchestrator) Roles() []string { return org.AssignableRoles() }

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

// PendingInvites are the org's invites not yet used or expired.
func (o *Orchestrator) PendingInvites(ctx context.Context, orgID string) ([]Invite, error) {
	return o.orgs.Pending(ctx, orgID, time.Now())
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
func (o *Orchestrator) RegisterInvited(
	ctx context.Context,
	token, email, password, name string,
) (*auth.Session, error) {
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
func (o *Orchestrator) BeginConnector(
	ctx context.Context,
	orgID, ghOrg string,
) (c Connector, action, manifest string, err error) {
	return o.conns.Begin(ctx, orgID, ghOrg)
}

// CompleteConnector takes GitHub's manifest callback.
func (o *Orchestrator) CompleteConnector(ctx context.Context, state, code string) (Connector, error) {
	return o.conns.Complete(ctx, state, code)
}

// ConnectorInstallURL is GitHub's install page for the app; "" while pending.
func (o *Orchestrator) ConnectorInstallURL(ctx context.Context, orgID, id string) (string, error) {
	return o.conns.InstallURL(ctx, orgID, id)
}

// ConnectorRepos asks GitHub what the app may read (the install check):
// empty means not installed yet, or installed on no repos.
func (o *Orchestrator) ConnectorRepos(ctx context.Context, orgID, id string) ([]Repo, error) {
	return o.conns.Repos(ctx, orgID, id)
}

// ConnectedConnectors is the org's connectors past GitHub's handshake.
func (o *Orchestrator) ConnectedConnectors(ctx context.Context, orgID string) ([]Connector, error) {
	return o.conns.ListConnected(ctx, orgID)
}

// OrgRepos is every repo the org's connected apps can read, for the setup
// wizard's install check and repo list. Two apps on one repo list it once,
// and an app GitHub will not answer for is skipped, as v0 did: the step
// then asks for an install rather than failing.
func (o *Orchestrator) OrgRepos(ctx context.Context, orgID string) ([]Repo, error) {
	cs, err := o.conns.ListConnected(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var out []Repo
	seen := map[string]bool{}
	for _, c := range cs {
		rs, err := o.conns.Repos(ctx, orgID, c.ID)
		if err != nil {
			slog.Warn("github repo list", "connector", c.Name, "err", err)
			continue
		}
		for _, r := range rs {
			if !seen[r.FullName] {
				seen[r.FullName] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func (o *Orchestrator) RenameConnector(ctx context.Context, orgID, id, name string) (Connector, error) {
	return o.conns.Rename(ctx, orgID, id, name)
}

func (o *Orchestrator) DeleteConnector(ctx context.Context, orgID, id string) error {
	return o.conns.Delete(ctx, orgID, id)
}
