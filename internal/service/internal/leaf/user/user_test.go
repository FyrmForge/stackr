package user_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
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

func seedOrg(t *testing.T, st *store.Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := st.Orgs.Create(ctx, store.Org{
		ID:        id,
		Name:      id,
		Slug:      id[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}); err != nil {
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

func TestFirstAccountIsAdmin(t *testing.T) {
	st := servicetest.Store(t)
	l := user.New(st.Users, st.Sessions, st.APIKeys)
	if _, err := l.Register(ctx, "a@x.io", "short", "a"); err == nil {
		t.Error("short password accepted")
	}
	first, err := l.Register(ctx, "A@x.io", "longenough", "a")
	if err != nil || first.Role != "admin" {
		t.Fatalf("first = %+v, %v; want admin", first, err)
	}
	second, err := l.Register(ctx, "b@x.io", "longenough", "b")
	if err != nil || second.Role != "user" {
		t.Fatalf("second = %+v, %v; want user", second, err)
	}
	if err := l.ChangePassword(ctx, second.ID, "wrong-one", "newpassword"); err == nil {
		t.Error("change without the current password accepted")
	}
	if err := l.ChangePassword(ctx, second.ID, "longenough", "newpassword"); err != nil {
		t.Fatal(err)
	}
	if err := l.SetPassword(ctx, second.ID, "x"); err == nil {
		t.Error("admin set a too-short password")
	}
	if _, err := l.Authenticate(ctx, "b@x.io", "newpassword"); err != nil {
		t.Errorf("login with the new password: %v", err)
	}
}

// B16: one "loses powers" rule for demote and disable. Both close every
// session and key; the last admin cannot lose them.
func TestLosePowers(t *testing.T) {
	st := servicetest.Store(t)
	l := user.New(st.Users, st.Sessions, st.APIKeys)
	a, b := seed(t, st, "a@x.io", true), seed(t, st, "b@x.io", true)
	org := seedOrg(t, st)

	if err := l.SetAdmin(ctx, b.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, lose := range []struct {
		name string
		do   func() error
	}{
		{"demote last admin", func() error { return l.SetAdmin(ctx, a.ID, false) }},
		{"disable last admin", func() error { return l.SetActive(ctx, a.ID, false) }},
	} {
		if err := lose.do(); err == nil {
			t.Errorf("%s accepted", lose.name)
		}
	}

	for _, lose := range []struct {
		name string
		do   func(id string) error
	}{
		{"demote", func(id string) error { return l.SetAdmin(ctx, id, false) }},
		{"disable", func(id string) error { return l.SetActive(ctx, id, false) }},
	} {
		if err := l.SetAdmin(ctx, b.ID, true); err != nil {
			t.Fatal(err)
		}
		if err := l.SetActive(ctx, b.ID, true); err != nil {
			t.Fatal(err)
		}
		tok, _, err := l.MintKey(ctx, b, org, "owner", "k")
		if err != nil {
			t.Fatal(err)
		}
		se := &auth.Session{
			ID:        uuid.NewString(),
			SubjectID: b.ID,
			Token:     uuid.NewString(),
			ExpiresAt: time.Now().Add(time.Hour),
			CreatedAt: time.Now(),
		}
		if err := st.Sessions.Create(ctx, se); err != nil {
			t.Fatal(err)
		}
		if err := lose.do(b.ID); err != nil {
			t.Fatalf("%s: %v", lose.name, err)
		}
		if _, _, err := l.ByKey(ctx, tok); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s left the key: %v", lose.name, err)
		}
		if got, _ := st.Sessions.GetByToken(ctx, se.Token); got != nil {
			t.Errorf("%s left the session", lose.name)
		}
	}
}
