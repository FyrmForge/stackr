package environment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func seedStack(t *testing.T, st *store.Store) string {
	t.Helper()
	org, id := uuid.NewString(), uuid.NewString()
	now := time.Now()
	if err := st.Orgs.Create(ctx, store.Org{ID: org, Name: org, Slug: org[:8], EnvColors: "{}", Settings: "{}", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Stacks.Create(ctx, store.Stack{ID: id, OrgID: org, Name: "s", Slug: "s", Settings: "{}", Domains: "[]", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return id
}

func branch(b string) environment.Spec {
	return environment.Spec{Type: environment.Static, FromKind: environment.FromBranch, FromBranch: b, Auto: true}
}

var promote = environment.Spec{Type: environment.Static, FromKind: environment.FromPromote}

func slugs(es []store.Environment) (out []string) {
	for _, e := range es {
		out = append(out, e.Slug)
	}
	return out
}

func TestLadder(t *testing.T) {
	st := servicetest.Store(t)
	l := environment.New(st.Environments, dockerfake.New())
	s := seedStack(t, st)

	if _, err := l.Create(ctx, s, "prod", promote); err == nil {
		t.Error("promote on the bottom rung accepted")
	}
	dev, err := l.Create(ctx, s, "Dev", branch("main"))
	if err != nil {
		t.Fatal(err)
	}
	staging, _ := l.Create(ctx, s, "staging", promote)
	prod, err := l.Create(ctx, s, "prod", promote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Create(ctx, s, "PROD", branch("x")); err == nil {
		t.Error("duplicate slug accepted")
	}
	if !environment.Above(prod, staging) || environment.Above(dev, staging) {
		t.Error("Above is wrong")
	}
	if b, err := l.Below(ctx, prod); err != nil || b.ID != staging.ID {
		t.Errorf("below prod = %s, %v", b.Slug, err)
	}
	if _, err := l.Below(ctx, dev); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("below the bottom = %v", err)
	}

	if err := l.Reorder(ctx, s, []string{prod.ID, dev.ID, staging.ID}); err == nil {
		t.Error("a promote env moved to the bottom")
	}
	if err := l.Reorder(ctx, s, []string{dev.ID, prod.ID}); err == nil {
		t.Error("partial order accepted")
	}
	if err := l.Reorder(ctx, s, []string{dev.ID, prod.ID, staging.ID}); err != nil {
		t.Fatal(err)
	}
	ladder, _ := l.Ladder(ctx, s)
	if got := slugs(ladder); len(got) != 3 || got[0] != "dev" || got[1] != "prod" || got[2] != "staging" {
		t.Errorf("ladder = %v", got)
	}

	if _, err := l.SetFrom(ctx, ladder[0], environment.FromPromote, "", true); err == nil {
		t.Error("bottom rung set to promote")
	}
	if _, err := l.SetFrom(ctx, ladder[2], environment.FromBranch, "", true); err == nil {
		t.Error("branch source without a branch accepted")
	}
	stg, err := l.SetFrom(ctx, ladder[2], environment.FromBranch, "release", false)
	if err != nil || stg.FromBranch != "release" || stg.Auto {
		t.Errorf("set from = %+v, %v", stg, err)
	}

	pr, err := l.CloneRow(ctx, dev, "pr-7", "feature")
	if err != nil || pr.Type != environment.Ephemeral || pr.BaseEnvID == nil || *pr.BaseEnvID != dev.ID || pr.Position != 3 {
		t.Errorf("clone row = %+v, %v", pr, err)
	}
}

func TestNetworkAndDelete(t *testing.T) {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	l := environment.New(st.Environments, fake)
	e, err := l.Create(ctx, seedStack(t, st), "dev", branch("main"))
	if err != nil {
		t.Fatal(err)
	}
	name, err := l.Network(ctx, e)
	if err != nil || name == "" {
		t.Fatal(name, err)
	}
	if err := l.Delete(ctx, e, 1); err == nil {
		t.Error("delete with tiles accepted")
	}
	fake.Err = map[string]error{"RemoveNetwork": errors.New("in use")}
	if err := l.Delete(ctx, e, 0); err == nil {
		t.Error("network failure swallowed")
	}
	if _, err := l.Get(ctx, e.ID); err != nil {
		t.Errorf("row gone after a failed network removal: %v", err)
	}
	fake.Err = nil
	if err := l.Delete(ctx, e, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"EnsureNetwork(" + name + ")", "RemoveNetwork(" + name + ")", "RemoveNetwork(" + name + ")"}
	calls := fake.Calls()
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i, c := range calls {
		if c.String() != want[i] {
			t.Errorf("call %d = %s, want %s", i, c, want[i])
		}
	}
}

func TestCloneRules(t *testing.T) {
	if environment.Reclaim(environment.Ephemeral) != "drop" || environment.Reclaim(environment.Static) != "detach" {
		t.Error("reclaim")
	}
	if got := environment.Rewrite("postgres://u:old@db/x", map[string]string{"old": "new"}); got != "postgres://u:new@db/x" {
		t.Errorf("rewrite = %s", got)
	}
}
