package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// One org, one stack, one env, one tile, and a link hanging off each scope.
// u1 is the member being demoted, u2 is somebody else in the same org.
type revokeStore struct {
	repo.Store
	links   []repo.SecretLink
	revoked map[string]bool
}

func newRevokeStore() *revokeStore {
	return &revokeStore{
		revoked: map[string]bool{},
		links: []repo.SecretLink{
			{ID: "org-u1", OwnerKind: repo.OwnerOrg, OwnerID: "o1", CreatedBy: "u1", State: repo.LinkOpen},
			{ID: "stack-u1", OwnerKind: repo.OwnerStack, OwnerID: "s1", CreatedBy: "u1", State: repo.LinkOpen},
			{ID: "env-u1", OwnerKind: repo.OwnerEnv, OwnerID: "e1", CreatedBy: "u1", State: repo.LinkOpen},
			{ID: "tile-u1", OwnerKind: repo.OwnerTile, OwnerID: "t1", CreatedBy: "u1", State: repo.LinkOpen},
			{ID: "tile-u2", OwnerKind: repo.OwnerTile, OwnerID: "t1", CreatedBy: "u2", State: repo.LinkOpen},
			{ID: "other-org-u1", OwnerKind: repo.OwnerOrg, OwnerID: "o2", CreatedBy: "u1", State: repo.LinkOpen},
		},
	}
}

func (r *revokeStore) ListOpenSecretLinks(context.Context) ([]repo.SecretLink, error) {
	return r.links, nil
}
func (r *revokeStore) ClaimSecretLink(_ context.Context, id, state string) (bool, error) {
	if state != repo.LinkRevoked {
		return false, nil
	}
	r.revoked[id] = true
	return true, nil
}
func (r *revokeStore) GetStack(_ context.Context, id string) (*repo.Stack, error) {
	if id == "boom" {
		return nil, errors.New("read failed")
	}
	if id != "s1" {
		return nil, nil
	}
	return &repo.Stack{ID: "s1", OrgID: "o1"}, nil
}
func (r *revokeStore) GetEnvironment(_ context.Context, id string) (*repo.Environment, error) {
	if id != "e1" {
		return nil, nil
	}
	return &repo.Environment{ID: "e1", StackID: "s1"}, nil
}
func (r *revokeStore) GetTile(_ context.Context, id string) (*repo.Tile, error) {
	if id != "t1" {
		return nil, nil
	}
	return &repo.Tile{ID: "t1", StackID: "s1"}, nil
}
func (r *revokeStore) ListOrgMembers(context.Context, string) ([]repo.OrgMember, error) {
	return []repo.OrgMember{{UserID: "u1"}, {UserID: "u2"}}, nil
}

// A share link is a bearer token: no session, no key, no role check at
// redeem. Demoting the person who minted it below write has to close it, or
// the demotion changed nothing about what they can still hand out.
func TestDemotionRevokesTheirShareLinks(t *testing.T) {
	st := newRevokeStore()
	svc := NewRevokeService(st, nil)
	if err := svc.MembershipChanged(context.Background(), "o1", "u1", "member", "viewer"); err != nil {
		t.Fatal(err)
	}
	got := st.revoked
	for _, id := range []string{"org-u1", "stack-u1", "env-u1", "tile-u1"} {
		if !got[id] {
			t.Errorf("%s survived the demotion", id)
		}
	}
	if got["tile-u2"] {
		t.Error("revoked a link somebody else minted")
	}
	if got["other-org-u1"] {
		t.Error("revoked their link in an org the change was not about")
	}
}

// Removal is a demotion to nothing at all.
func TestRemovalRevokesTheirShareLinks(t *testing.T) {
	st := newRevokeStore()
	if err := NewRevokeService(st, nil).MembershipChanged(context.Background(), "o1", "u1", "owner", ""); err != nil {
		t.Fatal(err)
	}
	if !st.revoked["tile-u1"] {
		t.Error("a removed member's links still work")
	}
}

// Minting is a write right, so an owner demoted to member keeps it. Revoking
// here would be a demotion taking away something the new role still grants.
func TestOwnerToMemberKeepsTheirShareLinks(t *testing.T) {
	st := newRevokeStore()
	if err := NewRevokeService(st, nil).MembershipChanged(context.Background(), "o1", "u1", "owner", "member"); err != nil {
		t.Fatal(err)
	}
	if len(st.revoked) != 0 {
		t.Errorf("owner -> member revoked %v", st.revoked)
	}
}

// Deactivation is not scoped to one org, so every link the account minted
// goes — including the one in the org the demotion tests deliberately spare.
func TestDeactivationRevokesEveryLinkTheyMinted(t *testing.T) {
	st := newRevokeStore()
	if err := NewRevokeService(st, nil).UserDeactivated(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"org-u1", "stack-u1", "env-u1", "tile-u1", "other-org-u1"} {
		if !st.revoked[id] {
			t.Errorf("%s survived the deactivation", id)
		}
	}
	if st.revoked["tile-u2"] {
		t.Error("revoked a link somebody else minted")
	}
}

// A move is the one change where the resource walks out from under everyone
// holding a link to it, so the creator does not come into it.
func TestStackMoveRevokesEverythingUnderIt(t *testing.T) {
	st := newRevokeStore()
	if err := NewRevokeService(st, nil).StackMoved(context.Background(), "s1", "o1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stack-u1", "env-u1", "tile-u1", "tile-u2"} {
		if !st.revoked[id] {
			t.Errorf("%s survived the move", id)
		}
	}
	if st.revoked["org-u1"] {
		t.Error("revoked an org-scoped link the move did not touch")
	}
}

// The services are where this has to happen: both surfaces call them, and a
// handler that forgot would be the second implementation this whole plan is
// about removing.
func TestMemberServiceRevokesOnSetRoleAndRemove(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(svc *MemberService) error
	}{
		{"demote", func(svc *MemberService) error {
			return svc.SetRole(ctx, &repo.Org{ID: "o1"}, "u1", "viewer")
		}},
		{"remove", func(svc *MemberService) error {
			return svc.Remove(ctx, &repo.Org{ID: "o1"}, "u1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &revokeMemberStore{revokeStore: newRevokeStore()}
			svc := NewMemberService(st, nil, NewRevokeService(st, nil))
			if err := tc.run(svc); err != nil {
				t.Fatal(err)
			}
			if !st.revoked["tile-u1"] {
				t.Error("the role change left their share links open")
			}
		})
	}
}

// revokeMemberStore is the revoke fixture plus the three member rows, so the
// member service can run its own path into the revoker.
type revokeMemberStore struct {
	*revokeStore
}

func (s *revokeMemberStore) GetOrgMember(_ context.Context, _, userID string) (*repo.OrgMember, error) {
	return &repo.OrgMember{OrgID: "o1", UserID: userID, Role: "member"}, nil
}
func (s *revokeMemberStore) UpsertOrgMember(context.Context, *repo.OrgMember) error { return nil }
func (s *revokeMemberStore) DeleteOrgMember(context.Context, string, string) error  { return nil }

// The same, through StackService.Move: the move is what point 15 left open
// alongside the demotion, and covering one without the other is the shape of
// bug this closes.
func TestStackServiceRevokesOnMove(t *testing.T) {
	st := &revokeStackStore{revokeStore: newRevokeStore()}
	svc := NewStackService(st, nil, nil, nil, nil, nil, NewRevokeService(st, nil))
	if err := svc.Move(context.Background(), &repo.Stack{ID: "s1", OrgID: "o1", Slug: "billing"}, "o2"); err != nil {
		t.Fatal(err)
	}
	if st.movedTo != "o2" {
		t.Fatalf("the stack did not move, got %q", st.movedTo)
	}
	if !st.revoked["tile-u2"] {
		t.Error("the move left links to the stack's tiles open")
	}
}

type revokeStackStore struct {
	*revokeStore
	movedTo string
}

func (s *revokeStackStore) GetStackBySlug(context.Context, string, string) (*repo.Stack, error) {
	return nil, nil
}
func (s *revokeStackStore) SetStackOrg(_ context.Context, _, orgID string) error {
	s.movedTo = orgID
	return nil
}

// One unreadable owner must not park the rest of the walk: the role change is
// already written by the time revocation runs, so stopping at the first error
// leaves working links behind and calls it a failure.
func TestOneBadOwnerDoesNotStopTheWalk(t *testing.T) {
	st := newRevokeStore()
	st.links = append([]repo.SecretLink{
		{ID: "broken", OwnerKind: repo.OwnerStack, OwnerID: "boom", CreatedBy: "u1", State: repo.LinkOpen},
	}, st.links...)
	err := NewRevokeService(st, nil).MembershipChanged(context.Background(), "o1", "u1", "member", "viewer")
	if err == nil {
		t.Error("the read failure was swallowed")
	}
	if !st.revoked["tile-u1"] {
		t.Error("a link after the broken one was left open")
	}
}
