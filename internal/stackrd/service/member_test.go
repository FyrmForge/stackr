package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type memberStore struct {
	repo.Store
	members []repo.OrgMember
	invites []repo.Invite
}

func (m *memberStore) ListOrgMembers(context.Context, string) ([]repo.OrgMember, error) {
	return m.members, nil
}
func (m *memberStore) CreateInvite(_ context.Context, i *repo.Invite) error {
	m.invites = append(m.invites, *i)
	return nil
}
func (m *memberStore) GetOrgMember(_ context.Context, _, userID string) (*repo.OrgMember, error) {
	for i := range m.members {
		if m.members[i].UserID == userID {
			return &m.members[i], nil
		}
	}
	return nil, nil
}
func (m *memberStore) UpsertOrgMember(context.Context, *repo.OrgMember) error { return nil }
func (m *memberStore) DeleteOrgMember(context.Context, string, string) error  { return nil }

// The role whitelist and the expiry bounds, which the panel and the API each
// had their own version of — and the API's folded a bad role to "member"
// silently, so `--role admin` granted less than was asked for and said so
// nowhere.
func TestInviteRules(t *testing.T) {
	ctx := context.Background()
	st := &memberStore{}
	svc := NewMemberService(st, nil, nil)
	org := &repo.Org{ID: "o1"}

	var bad svcerr.Invalid
	if _, err := svc.Invite(ctx, org, "a@b.test", "admin", 0, Actor{}); !errors.As(err, &bad) {
		t.Fatalf("an unknown role should be refused, got %v", err)
	}
	if _, err := svc.Invite(ctx, org, "", "member", 0, Actor{}); !errors.As(err, &bad) {
		t.Fatal("an empty address was accepted")
	}
	for _, days := range []int{-1, InviteDaysMax + 1} {
		if _, err := svc.Invite(ctx, org, "a@b.test", "member", days, Actor{}); !errors.As(err, &bad) {
			t.Errorf("%d days was accepted", days)
		}
	}
	inv, err := svc.Invite(ctx, org, "  A@B.test ", "", 0, Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "a@b.test" {
		t.Fatalf("address stored as %q", inv.Email)
	}
	if inv.Role != "member" {
		t.Fatalf("default role %q", inv.Role)
	}
	if d := inv.ExpiresAt.Sub(inv.CreatedAt).Hours() / 24; int(d+0.5) != InviteDaysDefault {
		t.Fatalf("default expiry %v days", d)
	}

	// Somebody already here is a role change, not a second invite.
	st.members = []repo.OrgMember{{UserID: "u1", Email: "A@B.test", Role: "member"}}
	var conflict svcerr.Conflict
	if _, err := svc.Invite(ctx, org, "a@b.test", "member", 0, Actor{}); !errors.As(err, &conflict) {
		t.Fatalf("re-inviting a member should conflict, got %v", err)
	}
}

// Removing somebody who is not a member is a 404 on both surfaces. The panel
// deleted nothing and reported success.
func TestRemoveUnknownMemberIsNotFound(t *testing.T) {
	svc := NewMemberService(&memberStore{}, nil, nil)
	err := svc.Remove(context.Background(), &repo.Org{ID: "o1"}, "nobody")
	if !errors.Is(err, svcerr.ErrNotFound) {
		t.Fatalf("want not-found, got %v", err)
	}
}
