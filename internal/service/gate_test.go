package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

var gateAcct = regexp.MustCompile(`"password":"([^"]+)","username":"([^"]+)"`)

// A tile address gated by secret refs unlocks with the secret's value, and
// changing the secret rebuilds the proxy rules with no container restart.
func TestGateSecretResolvesAndRebuilds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	scope := ParamScope{Kind: "env", ID: w.env}
	set := func(pw string) {
		_, err := w.orch.SetParams(ctx, scope, []ParamEntry{
			{Collection: "gate", Name: "user", Kind: "param", Value: "alice"},
			{Collection: "gate", Name: "password", Kind: "secret", Value: pw},
		})
		must(t, err)
	}
	set("one")
	_, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "${{ params.gate.user }}", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	check := func(pw string) {
		t.Helper()
		m := gateAcct.FindStringSubmatch(w.lastPush())
		if m == nil || m[2] != "alice" || bcrypt.CompareHashAndPassword([]byte(m[1]), []byte(pw)) != nil {
			t.Fatalf("push does not carry alice/%s: %v", pw, m)
		}
	}
	check("one")
	set("two")
	check("two")
}

// The panel names a gate's user, and why an address is locked.
func TestDomainGateLockedReason(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	d, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "alice", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	if g := w.orch.DomainGates(ctx, w.env, []string{d.ID})[d.ID]; g.User != "alice" || !strings.Contains(g.Locked, "gate.password") {
		t.Errorf("unset: %+v", g)
	}
	_, err = w.orch.SetParams(ctx, ParamScope{Kind: "env", ID: w.env}, []ParamEntry{{Collection: "gate", Name: "password", Kind: "secret", Value: "x"}})
	must(t, err)
	if g := w.orch.DomainGates(ctx, w.env, []string{d.ID})[d.ID]; g.User != "alice" || g.Locked != "" {
		t.Errorf("set: %+v", g)
	}
	if g := w.orch.DomainGates(ctx, w.env, []string{d.ID})[d.ID]; g.Locked != "" { // the kept verdict
		t.Errorf("kept: %+v", g)
	}
	_, err = w.orch.DeleteParam(ctx, ParamScope{Kind: "env", ID: w.env}, "gate", "password")
	must(t, err)
	if g := w.orch.DomainGates(ctx, w.env, []string{d.ID})[d.ID]; !strings.Contains(g.Locked, "gate.password") {
		t.Errorf("a kept verdict outlived its secret: %+v", g)
	}
}

// A gate user fed by a secret shows as the ref text, never the secret; an
// unknown domain is that row's lock, not an error for the rest.
func TestDomainGatesUserNeverResolvedSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	_, err := w.orch.SetParams(ctx, ParamScope{Kind: "env", ID: w.env}, []ParamEntry{
		{Collection: "gate", Name: "user", Kind: "secret", Value: "s3cr3t-user"},
		{Collection: "gate", Name: "password", Kind: "secret", Value: "pw"},
	})
	must(t, err)
	d, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "${{ params.gate.user }}", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	gs := w.orch.DomainGates(ctx, w.env, []string{d.ID, "missing"})
	if g := gs[d.ID]; g.User != "${{ params.gate.user }}" || g.Locked != "" || strings.Contains(g.User+g.Locked, "s3cr3t") {
		t.Errorf("gate = %+v", g)
	}
	if gs["missing"].Locked == "" {
		t.Error("an unreadable row is not locked")
	}
}

// X2: a PR env's gate password rotates through the pr block and the proxy is
// re-synced; a failed proxy sync still redeploys the readers and the save
// answers no error.
func TestGateSyncPRAndFailure(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	var last atomic.Value
	w := newWorld(t, WithProxy(func(_ context.Context, cfg json.RawMessage) error {
		if fail.Load() {
			return errors.New("proxy down")
		}
		last.Store(string(cfg))
		return nil
	}))
	ctx := context.Background()
	shared := ParamScope{Kind: "stack_pr", ID: w.stack}
	set := func(pw string) ([]Redeploy, error) {
		return w.orch.SetParams(ctx, shared, []ParamEntry{
			{Collection: "gate", Name: "user", Kind: "param", Value: "alice"},
			{Collection: "gate", Name: "password", Kind: "secret", Value: pw},
		})
	}
	_, err := set("one")
	must(t, err)
	pr := w.newEnv(t, "pr-1", "ephemeral")
	env, err := w.orch.envs.Get(ctx, pr)
	must(t, err)
	must(t, w.orch.seedPREnv(ctx, env))
	web := w.envTile(t, pr, "web", "params.gate.password")
	_, err = w.orch.AttachDomain(ctx, web.ID, DomainSpec{Host: "pr.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "${{ params.gate.user }}", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	_, err = set("two")
	must(t, err)
	if m := gateAcct.FindStringSubmatch(last.Load().(string)); m == nil || bcrypt.CompareHashAndPassword([]byte(m[1]), []byte("two")) != nil {
		t.Fatalf("the PR gate was not re-synced to the new password: %v", m)
	}
	fail.Store(true)
	rs, err := set("three")
	if err != nil {
		t.Fatalf("a failed proxy sync failed the save: %v", err)
	}
	if names(rs) != "shop/pr-1/web" {
		t.Errorf("readers after a failed sync = %q", names(rs))
	}
}
