package org_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func setup(t *testing.T) (*store.Store, *org.Leaf) {
	t.Helper()
	st := servicetest.Store(t)
	return st, org.New(st.Orgs, st.OrgMembers, st.Invites)
}

func seedUser(t *testing.T, st *store.Store, email string, admin bool) store.User {
	t.Helper()
	role := "user"
	if admin {
		role = "admin"
	}
	now := time.Now().UTC()
	u := store.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: "x",
		Name:         email,
		Role:         role,
		Active:       true,
		Theme:        "system",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := st.Users.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestDraftRenameDelete(t *testing.T) {
	st, l := setup(t)
	u := seedUser(t, st, "a@x.io", false)

	d, err := l.StartDraft(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.StartDraft(ctx, u.ID)
	if err != nil || again.ID != d.ID {
		t.Fatalf("second StartDraft = %s, %v; want the same draft", again.ID, err)
	}
	if _, err := l.SetupDone(ctx, d); err == nil {
		t.Error("setup finished under the placeholder name")
	}

	other, err := l.StartDraft(ctx, seedUser(t, st, "b@x.io", false).ID)
	if err != nil {
		t.Fatal(err)
	}
	if other, err = l.Rename(ctx, other, "Taken", nil); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		claims []org.Claim
	}{
		{"", nil},
		{"!!!", nil},
		{"Taken", nil},
		{"Shop", []org.Claim{{Host: "*.shop.example.com", OrgID: other.ID}}},
	} {
		if _, err := l.Rename(ctx, d, tt.name, tt.claims); err == nil {
			t.Errorf("rename to %q accepted", tt.name)
		}
	}
	// Our own domain is not a squat.
	d, err = l.Rename(ctx, d, "Shop", []org.Claim{{Host: "shop.example.com", OrgID: d.ID}})
	if err != nil || d.Slug != "shop" {
		t.Fatalf("rename = %+v, %v", d, err)
	}
	if d, err = l.SetupDone(ctx, d); err != nil || d.SetupDoneAt == nil {
		t.Fatalf("SetupDone: %v", err)
	}

	if err := l.Delete(ctx, d, 1); err == nil {
		t.Error("deleted an org with stacks")
	}
	if err := l.Delete(ctx, other, 0); err != nil { // a draft is exempt from last-org
		t.Fatal(err)
	}
	if err := l.Delete(ctx, d, 0); err == nil {
		t.Error("deleted the last finished org")
	}
}

func TestLastOwner(t *testing.T) {
	st, l := setup(t)
	a, b := seedUser(t, st, "a@x.io", false), seedUser(t, st, "b@x.io", false)
	o, err := l.StartDraft(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.RemoveMember(ctx, o.ID, a.ID); err == nil {
		t.Error("removed the last owner")
	}
	if _, err := l.RemoveMember(ctx, o.ID, b.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("remove non-member = %v, want ErrNotFound", err)
	}
	if err := l.AddMember(ctx, o.ID, b.ID, "admin"); err == nil {
		t.Error("typo role folded instead of refused")
	}
	if err := l.AddMember(ctx, o.ID, b.ID, org.Owner); err != nil {
		t.Fatal(err)
	}
	if role, err := l.RemoveMember(ctx, o.ID, a.ID); err != nil || role != org.Owner {
		t.Errorf("remove with another owner = %q, %v", role, err)
	}
}

// B14, B15: one invite path, expiry capped, burned atomically before the
// join, refused once used; two racing accepts give exactly one member row.
func TestInvite(t *testing.T) {
	st, l := setup(t)
	owner, joiner := seedUser(t, st, "o@x.io", false), seedUser(t, st, "j@x.io", false)
	o, _ := l.StartDraft(ctx, owner.ID)
	now := time.Now().UTC()

	if _, err := l.Invite(ctx, o.ID, "o@x.io", "", owner.ID, true, now); err == nil {
		t.Error("invited an existing member")
	}
	inv, err := l.Invite(ctx, o.ID, " J@X.io ", "", owner.ID, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.ID) != 48 || inv.ExpiresAt.Sub(now) != org.InviteTTL {
		t.Errorf("invite token %q, ttl %v", inv.ID, inv.ExpiresAt.Sub(now))
	}
	if _, err := l.Accept(ctx, inv.ID, joiner.ID, "j@x.io", now.Add(org.InviteTTL)); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("expired accept = %v", err)
	}
	if _, err := l.Accept(ctx, inv.ID, owner.ID, "o@x.io", now); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("accept by another address = %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Go(func() {
			results[i] = st.Tx(ctx, func(tx store.Tx) error {
				_, err := org.New(tx.Orgs, tx.OrgMembers, tx.Invites).Accept(ctx, inv.ID, joiner.ID, "j@x.io", now)
				return err
			})
		})
	}
	wg.Wait()
	if (results[0] == nil) == (results[1] == nil) {
		t.Errorf("racing accepts: %v / %v, want exactly one success", results[0], results[1])
	}
	ms, _ := l.Members(ctx, o.ID)
	if len(ms) != 2 {
		t.Errorf("%d members, want 2", len(ms))
	}
	if _, err := l.Accept(ctx, inv.ID, joiner.ID, "j@x.io", now); err == nil {
		t.Error("used invite accepted again")
	}
}
