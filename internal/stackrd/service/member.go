package service

import (
	"context"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	svcmail "github.com/FyrmForge/stackr/internal/stackrd/service/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// MemberService owns who is in an organization and at what level.
//
// Every rule here existed twice and disagreed. An invite lasted between 1 and
// 365 days on the panel, defaulting to 14, and on the API any number at all,
// defaulting to 7 — including a negative one, which minted an invite that had
// already expired. A bad role was a 400 on the panel and silently became
// `member` over the API, so `--role admin` from the CLI quietly granted less
// than the operator asked for and said nothing. Removing a member who was not
// one answered 404 on the API and "Member removed." on the panel. And only
// the API refused to invite somebody who was already here.
type MemberService struct {
	store  repo.Store
	mail   *svcmail.Service
	revoke *RevokeService
}

func NewMemberService(store repo.Store, mail *svcmail.Service, revoke *RevokeService) *MemberService {
	return &MemberService{store: store, mail: mail, revoke: revoke}
}

// Invite expiry, one rule for both surfaces.
const (
	// InviteDaysDefault is what an invite lasts when nobody says. Two weeks:
	// long enough to survive a holiday, short enough that a forgotten link
	// in an inbox stops working.
	InviteDaysDefault = 14
	// InviteDaysMax is the cap. Unbounded meant an invite good for a decade,
	// which is a credential, not an invitation.
	InviteDaysMax = 365
)

// Role is the level a member holds. The whitelist is here so a typo is
// refused rather than quietly downgraded.
func validOrgRole(r string) bool {
	switch r {
	case "owner", "member", "viewer":
		return true
	}
	return false
}

// Invite creates an invitation and mails it when this server can send mail.
// The caller checks that the actor may invite; who is in an org is this
// service's business, who is allowed to change that is not.
func (s *MemberService) Invite(ctx context.Context, org *repo.Org, email, role string, days int, by Actor) (*repo.Invite, error) {
	if org == nil {
		return nil, svcerr.ErrNotFound
	}
	email = NormalizeEmail(email)
	if email == "" {
		return nil, invalid("email", "required")
	}
	if role == "" {
		role = "member"
	}
	if !validOrgRole(role) {
		// Refused, not folded to "member". Folding is how `--role admin`
		// granted member access and reported success.
		return nil, invalid("role", "must be owner, member or viewer")
	}
	if days == 0 {
		days = InviteDaysDefault
	}
	if days < 1 || days > InviteDaysMax {
		return nil, svcerr.Invalidf("expires_days", "must be between 1 and %d days", InviteDaysMax)
	}
	// An existing member's role is changed, not re-invited: a second invite
	// to somebody who is already here reads as an error the caller cannot
	// see. Only the API checked.
	ms, err := s.store.ListOrgMembers(ctx, org.ID)
	if err != nil {
		return nil, err
	}
	for i := range ms {
		if strings.EqualFold(ms[i].Email, email) {
			return nil, svcerr.Conflictf("%s is already a member; change their role instead", email)
		}
	}
	now := time.Now().UTC()
	inv := &repo.Invite{
		ID: secrets.RandomHex(24), OrgID: org.ID, Email: email, Role: role,
		CreatedBy: by.ID, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, days),
	}
	if err := s.store.CreateInvite(ctx, inv); err != nil {
		return nil, err
	}
	// A bounce is not fatal: the link comes back to the caller either way.
	inv.MailFailed = s.mail.SendInvite(ctx, org, inv)
	return inv, nil
}

// SetRole changes a member's level.
func (s *MemberService) SetRole(ctx context.Context, org *repo.Org, userID, role string) error {
	if org == nil {
		return svcerr.ErrNotFound
	}
	if !validOrgRole(role) {
		return invalid("role", "must be owner, member or viewer")
	}
	m, err := s.store.GetOrgMember(ctx, org.ID, userID)
	if err != nil || m == nil {
		return svcerr.ErrNotFound
	}
	if m.Role == "owner" && role != "owner" {
		if err := s.lastOwnerGuard(ctx, org, userID); err != nil {
			return err
		}
	}
	was := m.Role
	m.Role = role
	if err := s.store.UpsertOrgMember(ctx, m); err != nil {
		return err
	}
	// A demotion is not finished when the row is written. What the old role
	// let them mint outlives it — see RevokeService.
	return s.revoke.MembershipChanged(ctx, org.ID, userID, was, role)
}

// Remove takes a member out. A member who was not one is a 404 on both
// surfaces now; the panel used to report success.
func (s *MemberService) Remove(ctx context.Context, org *repo.Org, userID string) error {
	if org == nil {
		return svcerr.ErrNotFound
	}
	m, err := s.store.GetOrgMember(ctx, org.ID, userID)
	if err != nil || m == nil {
		return svcerr.ErrNotFound
	}
	if m.Role == "owner" {
		if err := s.lastOwnerGuard(ctx, org, userID); err != nil {
			return err
		}
	}
	if err := s.store.DeleteOrgMember(ctx, org.ID, userID); err != nil {
		return err
	}
	return s.revoke.MembershipChanged(ctx, org.ID, userID, m.Role, "")
}

// lastOwnerGuard refuses to leave an org with nobody who can administer it.
// A server admin can still reach it, but from inside the product the org
// would be stuck: nobody left to invite anybody.
//
// Fails closed on a read error: refusing a demotion that would have been fine
// is recoverable, and the other way round is not.
func (s *MemberService) lastOwnerGuard(ctx context.Context, org *repo.Org, userID string) error {
	members, err := s.store.ListOrgMembers(ctx, org.ID)
	if err != nil {
		return svcerr.Conflictf("an organization needs at least one owner")
	}
	for _, m := range members {
		if m.Role == "owner" && m.UserID != userID {
			return nil
		}
	}
	return svcerr.Conflictf("an organization needs at least one owner")
}

// RoleOf is this user's role in this organization, "" when they are not a
// member. A server admin is not a member row and gets "" here too: admin-ness
// is a property of the user, which every caller already has.
//
// AccessService.Principal builds the same answer for every org the user is in,
// in one pass, and keeps doing so. That is a different question — "what can
// this request do anywhere" versus "what is this one person to this one org" —
// and the only thing the two share is a table read, not a rule.
func (s *MemberService) RoleOf(ctx context.Context, orgID, userID string) (string, error) {
	m, err := s.store.GetOrgMember(ctx, orgID, userID)
	if err != nil || m == nil {
		return "", err
	}
	return m.Role, nil
}

// ListMembers is an organization's members.
func (s *MemberService) ListMembers(ctx context.Context, orgID string) ([]repo.OrgMember, error) {
	return s.store.ListOrgMembers(ctx, orgID)
}

// ListInvites is an organization's outstanding invitations.
func (s *MemberService) ListInvites(ctx context.Context, orgID string) ([]repo.Invite, error) {
	return s.store.ListInvitesByOrg(ctx, orgID)
}
