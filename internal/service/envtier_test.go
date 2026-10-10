package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// D3: joining a locked tier (by create or rename) needs tier.write; an
// unlocked tier and a plain slug stay open.
func TestJoinsLockedTier(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	org := env.Org(t, "acme")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.CreateTier(ctx, org, "prod"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.CreateTier(ctx, org, "qa"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetTierLock(ctx, org, "qa", false); err != nil {
		t.Fatal(err)
	}
	as := func(role string) *service.Principal {
		return &service.Principal{Access: authz.User{ID: "u", Active: true, Roles: map[string]string{org: role}}}
	}
	check := func(role, name, cur string) error {
		return env.Orch.JoinsLockedTier(ctx, as(role), st.ID, name, cur)
	}
	if err := check("member", "Prod", ""); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "prod is a locked tier; an org owner adds envs to it") {
		t.Errorf("member creates prod = %v, want refused", err)
	}
	if err := check("member", "prod", "dev"); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("member renames to prod = %v, want refused", err)
	}
	if err := check("owner", "prod", ""); err != nil {
		t.Errorf("owner creates prod = %v", err)
	}
	if err := check("member", "qa", ""); err != nil {
		t.Errorf("member joins an unlocked tier = %v", err)
	}
	if err := check("member", "dev", ""); err != nil {
		t.Errorf("member makes a plain env = %v", err)
	}
	if err := check("member", "prod", "prod"); err != nil {
		t.Errorf("an env staying in its tier = %v", err)
	}
}

// D3: the stack file does not make an env that joins a locked tier.
func TestPushSkipsLockedTierEnv(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	conn := env.Connector(t, org, "whsec")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetConfigRepo(ctx, st.ID, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.CreateTier(ctx, org, "prod"); err != nil {
		t.Fatal(err)
	}
	sha := g.Commit(t, "acme/shop", "main", map[string]string{"stackr-compose.yml": "version: 1\nstack: shop\nladder:\n  - dev\n  - prod\nhead: main\n"})
	body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `",` +
		`"repository":{"clone_url":"https://github.com/acme/shop.git","default_branch":"main"},` +
		`"commits":[{"modified":["stackr-compose.yml"]}]}`)
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	if err := env.Orch.Webhook(ctx, conn, "push", "sha256="+hex.EncodeToString(mac.Sum(nil)), body); err != nil {
		t.Fatal(err)
	}
	eventually(t, "dev made", func() bool {
		es, _ := env.Orch.Ladder(ctx, st.ID)
		return len(es) == 1 && es[0].Slug == "dev" && es[0].ReleaseID != nil
	})
	if es, _ := env.Orch.Ladder(ctx, st.ID); len(es) != 1 {
		t.Errorf("ladder = %+v, want dev only (prod is a locked tier)", es)
	}
}
