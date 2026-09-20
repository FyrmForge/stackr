package service

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// RevokeService closes what a live check cannot reach when somebody's
// standing changes.
//
// Point 15 left this open. Deactivating a user is mostly self-healing: the
// session's subject loader (handlers/web/server.go) and the API key
// authenticator (handlers/api/v1/auth.go) both re-read the user on every
// request, so a deactivation is felt at the next one — everything except the
// share links it minted, which UserDeactivated now closes. Demoting a member,
// removing them, and moving a stack to another org felt like the same kind of
// event and did nothing at all.
//
// Most of that is already covered by the same re-check-at-the-boundary rule:
//
//   - Sessions need nothing. `middleware.OrgContext` re-resolves the user's
//     orgs and their role in the active one per request, and an org cookie
//     that no longer matches falls back to one they are still in.
//   - API keys need nothing for the org that changed. A key carries scopes,
//     never a role; `AccessService.Require` reads the role live out of the
//     target org, so a demoted key is refused at the same instant a demoted
//     session is. Deleting the key would punish the user's other orgs, and
//     narrowing its scopes is the mint-time question point 15 left for the
//     dev — see 05-assumptions.md.
//
// What cannot re-check is what this service handles:
//
//   - Secret links. A share link is a bearer token: whoever holds the URL
//     reads the values it points at, with no session, no key and no role
//     check anywhere in the redeem path. A member who minted one and was then
//     demoted to viewer or removed outright has left a working door open.
//   - Websocket rooms, which are checked when they are joined and never
//     again. That one is a nudge, not a teardown — see notify.AccessChanged.
type RevokeService struct {
	store    repo.Store
	notifier *notify.Notifier
}

func NewRevokeService(store repo.Store, n *notify.Notifier) *RevokeService {
	return &RevokeService{store: store, notifier: n}
}

// MembershipChanged reacts to a role change or a removal. `to` is "" when the
// member is gone.
//
// A drop below write is what matters: minting a share link is VerbShareLinkMint,
// which sits on LevelWrite, so an owner demoted to member keeps rights they
// already had and their links stand. Everything else is a nudge, because a
// viewer still reads the org and still belongs in its room.
func (s *RevokeService) MembershipChanged(ctx context.Context, orgID, userID, from, to string) error {
	if s == nil {
		return nil // wiring optional in tests, as with the notifier
	}
	// Nudged on every path, the early return included: a viewer re-joining
	// rooms they still hold is a no-op, and the alternative is deciding twice
	// whether a role change moved a room.
	defer s.notifier.AccessChanged(userID)
	fromLevel, _ := RoleLevel(from)
	toLevel, ok := RoleLevel(to)
	if !ok {
		toLevel = LevelRead // removed: no standing at all
	}
	if fromLevel < LevelWrite || toLevel >= LevelWrite {
		return nil
	}
	return s.revoke(ctx, func(l *repo.SecretLink, linkOrg, _ string) bool {
		return l.CreatedBy == userID && linkOrg == orgID
	})
}

// UserDeactivated reacts to an admin disabling an account.
//
// Sessions and API keys need nothing — both re-read the user per request and
// refuse a deactivated one at the next hop. Share links are the exception for
// the same reason they are in MembershipChanged: they are bearer tokens with
// no user on the redeem path at all, so a disabled account's drop boxes keep
// answering until the rows are closed.
//
// Every link the user minted goes, in every org: deactivation is not scoped to
// one org the way a demotion is. Decided in 06-points-18-20.md, "The four open
// decisions", #3.
func (s *RevokeService) UserDeactivated(ctx context.Context, userID string) error {
	if s == nil {
		return nil // wiring optional in tests, as with the notifier
	}
	defer s.notifier.AccessChanged(userID)
	return s.revoke(ctx, func(l *repo.SecretLink, _, _ string) bool {
		return l.CreatedBy == userID
	})
}

// StackMoved reacts to a stack changing org.
//
// Every open link under the stack goes, whoever minted it: the links were
// minted by people in the old org and now point at resources the new org
// owns, so neither side's membership answers for them. The old org's members
// are nudged because their sockets are joined to the stack's room.
func (s *RevokeService) StackMoved(ctx context.Context, stackID, fromOrgID string) error {
	if s == nil {
		return nil // wiring optional in tests, as with the notifier
	}
	if ms, err := s.store.ListOrgMembers(ctx, fromOrgID); err == nil {
		for i := range ms {
			s.notifier.AccessChanged(ms[i].UserID)
		}
	}
	return s.revoke(ctx, func(_ *repo.SecretLink, _, linkStack string) bool {
		return linkStack == stackID
	})
}

// revoke walks the live links and closes the ones match picks. The row is
// kept — it names fields but never holds values, so it is the audit line for
// the exchange, which is why this is a state flip and not a delete.
func (s *RevokeService) revoke(ctx context.Context, match func(l *repo.SecretLink, orgID, stackID string) bool) error {
	links, err := s.store.ListOpenSecretLinks(ctx)
	if err != nil {
		return err
	}
	// Every link the match picks gets its claim attempted, and the failures
	// are reported together. Returning on the first one would leave the rest
	// of the list open while the role change it belongs to had already been
	// written — a half-revocation reported as a failure.
	var errs []error
	for i := range links {
		l := &links[i]
		orgID, stackID, err := s.ownerOrg(ctx, l.OwnerKind, l.OwnerID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if orgID == "" || !match(l, orgID, stackID) {
			continue
		}
		if _, err := s.store.ClaimSecretLink(ctx, l.ID, repo.LinkRevoked); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ownerOrg walks a link's scope up to its org, returning the stack on the way
// when there is one. An owner that no longer exists answers "", which the
// caller skips: a link pointing at a deleted scope has nothing to reveal.
//
// This is the resolver point 18 calls TenancyOf, in the one shape needed
// today. It is not that resolver — that one takes the request's id kinds and
// has to answer for every route — and is deliberately not exported.
func (s *RevokeService) ownerOrg(ctx context.Context, kind, id string) (orgID, stackID string, err error) {
	switch kind {
	case repo.OwnerOrg:
		return id, "", nil
	case repo.OwnerStack:
		st, err := s.store.GetStack(ctx, id)
		if err != nil || st == nil {
			return "", "", err
		}
		return st.OrgID, st.ID, nil
	case repo.OwnerEnv:
		env, err := s.store.GetEnvironment(ctx, id)
		if err != nil || env == nil {
			return "", "", err
		}
		return s.ownerOrg(ctx, repo.OwnerStack, env.StackID)
	case repo.OwnerTile:
		t, err := s.store.GetTile(ctx, id)
		if err != nil || t == nil {
			return "", "", err
		}
		return s.ownerOrg(ctx, repo.OwnerStack, t.StackID)
	}
	return "", "", nil
}
