package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seed(t *testing.T, st *store.Store, email string, admin bool) store.User {
	t.Helper()
	role := "user"
	if admin {
		role = "admin"
	}
	now := time.Now().UTC()
	u := store.User{ID: uuid.NewString(), Email: email, PasswordHash: "x", Name: email, Role: role,
		Active: true, Theme: "system", CreatedAt: now, UpdatedAt: now}
	if err := st.Users.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{ID: id, Name: id, Slug: id[:8], EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return id
}

// B36: a key never holds more than its minter.
func TestMintKey(t *testing.T) {
	st := servicetest.Store(t)
	l := user.New(st.Users, st.Sessions, st.APIKeys)
	u, admin := seed(t, st, "u@x.io", false), seed(t, st, "a@x.io", true)
	org := seedOrg(t, st)

	if _, _, err := l.MintKey(ctx, u, "", "", "k"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("non-admin unbound key = %v, want refused", err)
	}
	if _, _, err := l.MintKey(ctx, u, org, "", "k"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("mint into an org the minter is not in = %v", err)
	}
	tok, k, err := l.MintKey(ctx, u, org, "owner", "k")
	if err != nil || k.OrgID == nil || *k.OrgID != org {
		t.Fatalf("mint = %+v, %v", k, err)
	}
	if _, got, err := l.ByKey(ctx, tok); err != nil || got.ID != k.ID || got.TokenHash == tok {
		t.Errorf("ByKey = %+v, %v", got, err)
	}
	if _, _, err := l.MintKey(ctx, admin, "", "", "k"); err != nil {
		t.Errorf("admin unbound key: %v", err)
	}
	if err := l.RevokeKey(ctx, admin.ID, k.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("revoke someone else's key = %v", err)
	}
}
