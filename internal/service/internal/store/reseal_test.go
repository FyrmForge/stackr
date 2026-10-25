package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A rotation moves every sealed table and sealed setting to the new key in
// one go: the old key opens nothing afterwards.
func TestResealMovesEverySealedValue(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	mustSeedOrg(t, s)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Credentials.Create(ctx, store.Credential{ID: "c1", OrgID: "o1", Name: "n", Password: "hunter2", CreatedAt: t0}))
	must(s.Params.Create(ctx, store.Param{ID: "p1", ScopeKind: "server", Name: "K", Kind: "secret", Value: "s3cret", CreatedAt: t0, UpdatedAt: t0}))
	must(s.Settings.SetSealed(ctx, store.CanaryKey, store.CanaryValue))
	must(s.Settings.Set(ctx, "plain", "visible"))

	oldBox, err := secrets.New(servicetest.Key)
	must(err)
	nextBox, err := secrets.New(strings.Repeat("cd", 32))
	must(err)
	must(s.Reseal(ctx, oldBox, nextBox))

	n := store.New(s.DB(), nextBox)
	if c, err := n.Credentials.Get(ctx, "c1"); err != nil || c.Password != "hunter2" {
		t.Errorf("credential under the new key = %q, %v", c.Password, err)
	}
	if p, err := n.Params.Get(ctx, "p1"); err != nil || p.Value != "s3cret" {
		t.Errorf("param under the new key = %q, %v", p.Value, err)
	}
	if v, err := n.Settings.GetSealed(ctx, store.CanaryKey); err != nil || v != store.CanaryValue {
		t.Errorf("canary under the new key = %q, %v", v, err)
	}
	if _, err := s.Credentials.Get(ctx, "c1"); err == nil {
		t.Error("the old key still opens a credential")
	}
	if v, _, _ := n.Settings.Get(ctx, "plain"); v != "visible" {
		t.Errorf("plain setting = %q", v)
	}
}
