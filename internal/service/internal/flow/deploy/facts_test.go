package deploy

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

type snapWorld struct {
	f     *Flow
	st    store.Stack
	envs  map[string]store.Environment
	tiers map[string]store.Tier
}

// snapWorld: one org and stack with envs dev and prod (named like tiers),
// demo (off-tier) and pr-1 (a PR env). Tiers are added per test.
func newSnapWorld(t *testing.T) *snapWorld {
	t.Helper()
	ctx := context.Background()
	s := storetest.Store(t)
	now := time.Now()
	must(t, s.Orgs.Create(ctx, store.Org{ID: "o", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	st := store.Stack{ID: "s", OrgID: "o", Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}
	must(t, s.Stacks.Create(ctx, st))
	w := &snapWorld{
		st:    st,
		envs:  map[string]store.Environment{},
		tiers: map[string]store.Tier{},
		f: &Flow{
			Envs:   environment.New(s.Environments, dockerfake.New()),
			Params: params.New(s.Params),
			Tiers:  tier.New(s.Tiers),
		},
	}
	for i, e := range []store.Environment{
		{Slug: "dev"}, {Slug: "prod"}, {Slug: "demo"}, {Slug: "pr-1", Type: environment.Ephemeral},
	} {
		e.ID, e.StackID, e.Name, e.Position = "e-"+e.Slug, "s", e.Slug, i
		if e.Type == "" {
			e.Type = environment.Static
		}
		e.Settings, e.Network, e.FromKind, e.FromBranch, e.CreatedAt = "{}", "n-"+e.Slug, "branch", "main", now
		must(t, s.Environments.Create(ctx, e))
		w.envs[e.Slug] = e
	}
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (w *snapWorld) tier(t *testing.T, slug string, locked bool) {
	t.Helper()
	tr, err := w.f.Tiers.Create(context.Background(), "o", slug)
	must(t, err)
	if !locked {
		tr, err = w.f.Tiers.SetLocked(context.Background(), tr, false)
		must(t, err)
	}
	w.tiers[slug] = tr
}

func (w *snapWorld) set(t *testing.T, kind, id, name, val string) {
	t.Helper()
	must(t, w.f.Params.Set(context.Background(), params.Scope{Kind: kind, ID: id},
		params.Entry{Collection: "c", Name: name, Kind: params.Param, Value: val}))
}

func (w *snapWorld) snap(t *testing.T, env string) params.Snapshot {
	t.Helper()
	s, err := w.f.ParamSnapshot(context.Background(), w.envs[env], w.st, true)
	must(t, err)
	return s
}

func has(m map[string]params.Value, key, want string) bool { return m[key].V == want }

// An org with no tiers reads its org scope; the stack and org pr blocks are
// always in Other, and no tier block exists.
func TestSnapshotUntieredOrg(t *testing.T) {
	w := newSnapWorld(t)
	w.set(t, "org", "o", "n", "org")
	w.set(t, "env", w.envs["dev"].ID, "n", "dev")
	w.set(t, "env", w.envs["prod"].ID, "n", "prod")
	s := w.snap(t, "dev")
	if s.Tier != "" || s.Env != "dev" || !has(s.OrgParams, "c.n", "org") || !has(s.EnvParams, "c.n", "dev") {
		t.Errorf("own = tier %q env %q org %v env %v", s.Tier, s.Env, s.OrgParams, s.EnvParams)
	}
	for _, k := range []string{"stack:pr", "org:pr", "stack:dev", "stack:prod", "stack:demo"} {
		if _, ok := s.Other[k]; !ok {
			t.Errorf("Other lacks %s", k)
		}
	}
	if _, ok := s.Other["stack:pr-1"]; ok {
		t.Error("a PR env is listed in Other")
	}
	if b := s.Other["stack:prod"]; b.Locked || !has(b.Values, "c.n", "prod") {
		t.Errorf("stack:prod = %+v, want its own lock (the fixture envs are unlocked)", b)
	}
}

// A tiered env reads its tier's block; a tier and an env named like it take
// the tier's lock, not their own column.
func TestSnapshotTieredEnv(t *testing.T) {
	w := newSnapWorld(t)
	w.tier(t, "dev", false)
	w.tier(t, "prod", true)
	w.set(t, "org", "o", "n", "org")
	w.set(t, "tier", w.tiers["dev"].ID, "n", "tdev")
	w.set(t, "tier", w.tiers["prod"].ID, "n", "tprod")
	w.set(t, "env", w.envs["dev"].ID, "n", "edev")
	w.set(t, "env", w.envs["prod"].ID, "n", "eprod")
	w.set(t, "env", w.envs["demo"].ID, "n", "edemo")
	s := w.snap(t, "dev")
	if s.Tier != "dev" || !has(s.OrgParams, "c.n", "tdev") || !has(s.EnvParams, "c.n", "edev") {
		t.Errorf("own = tier %q org %v env %v", s.Tier, s.OrgParams, s.EnvParams)
	}
	if b := s.Other["org:prod"]; !b.Locked || b.Values != nil {
		t.Errorf("org:prod = %+v, want locked with no values", b)
	}
	if b := s.Other["org:dev"]; b.Locked || !has(b.Values, "c.n", "tdev") {
		t.Errorf("org:dev = %+v", b)
	}
	if b := s.Other["stack:prod"]; !b.Locked || b.Values != nil {
		t.Errorf("stack:prod = %+v, want the tier's lock", b)
	}
	if b := s.Other["stack:demo"]; b.Locked || !has(b.Values, "c.n", "edemo") {
		t.Errorf("stack:demo = %+v, want its own lock (unlocked)", b)
	}
	if _, ok := s.Other["org:demo"]; ok {
		t.Error("an env that is no tier has an org block")
	}
}

// An off-tier env of a tiered org reads no org block; a locked off-tier env
// is locked to others.
func TestSnapshotOffTierEnv(t *testing.T) {
	w := newSnapWorld(t)
	w.tier(t, "dev", false)
	w.set(t, "tier", w.tiers["dev"].ID, "n", "tdev")
	w.set(t, "env", w.envs["demo"].ID, "n", "edemo")
	s := w.snap(t, "demo")
	if s.Tier != "" || s.OrgParams != nil || !has(s.EnvParams, "c.n", "edemo") {
		t.Errorf("own = tier %q org %v env %v", s.Tier, s.OrgParams, s.EnvParams)
	}
	if _, ok := s.Other["org:dev"]; !ok {
		t.Error("an unlocked tier is not readable from an off-tier env")
	}
	_, err := w.f.Envs.SetLocked(context.Background(), w.envs["demo"], true)
	must(t, err)
	if b := w.snap(t, "dev").Other["stack:demo"]; !b.Locked || b.Values != nil {
		t.Errorf("stack:demo = %+v, want locked with no values", b)
	}
}

// A PR env reads its own env scope (a copy of stack_pr) and the org_pr block,
// and still sees every other block through Other.
func TestSnapshotPREnv(t *testing.T) {
	w := newSnapWorld(t)
	w.tier(t, "dev", false)
	w.set(t, "tier", w.tiers["dev"].ID, "n", "tdev")
	w.set(t, "env", w.envs["pr-1"].ID, "n", "own")
	w.set(t, "stack_pr", "s", "n", "spr")
	w.set(t, "org_pr", "o", "n", "opr")
	s := w.snap(t, "pr-1")
	if s.Tier != "" || !has(s.EnvParams, "c.n", "own") || !has(s.OrgParams, "c.n", "opr") {
		t.Errorf("own = tier %q env %v org %v", s.Tier, s.EnvParams, s.OrgParams)
	}
	if b := s.Other["stack:pr"]; b.Locked || !has(b.Values, "c.n", "spr") {
		t.Errorf("stack:pr = %+v", b)
	}
	if b := s.Other["org:pr"]; b.Locked || !has(b.Values, "c.n", "opr") {
		t.Errorf("org:pr = %+v", b)
	}
	if b := s.Other["org:dev"]; !has(b.Values, "c.n", "tdev") {
		t.Errorf("org:dev = %+v, an unlocked tier is readable from a PR env", b)
	}
}
