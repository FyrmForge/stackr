package service

import (
	"time"

	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func (w *world) deploys(t *testing.T, tl Tile) int {
	t.Helper()
	js, err := w.orch.TileJobs(context.Background(), []string{tl.ID}, 10)
	must(t, err)
	return len(js)
}

func (w *world) setEnv(t *testing.T, tl Tile, ref string) {
	t.Helper()
	_, err := w.st.DB().ExecContext(context.Background(), `UPDATE tiles SET env_json = ? WHERE id = ?`,
		`{"V":"${{ `+ref+` }}"}`, tl.ID)
	must(t, err)
}

// Lock and tier changes redeploy the running tiles whose view moved: the
// first tier switches org.params readers to tier blocks, a lock moves only
// the [x] readers, a rename the old and new slug, the last delete puts the
// org readers back.
func TestTierChangesRedeploy(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	plain, qual := w.tile(t, "plain", true), w.tile(t, "qual", true)
	w.setEnv(t, plain, "org.params.c.n")
	w.setEnv(t, qual, "org.params.c[prod].n")

	_, err := w.orch.CreateTier(ctx, w.org, "prod")
	must(t, err)
	if w.deploys(t, plain) != 1 || w.deploys(t, qual) != 0 {
		t.Fatalf("first tier: plain %d qual %d, want 1 0", w.deploys(t, plain), w.deploys(t, qual))
	}
	_, err = w.orch.SetTierLock(ctx, w.org, "prod", false)
	must(t, err)
	if w.deploys(t, plain) != 1 || w.deploys(t, qual) != 1 {
		t.Fatalf("unlock: plain %d qual %d, want 1 1", w.deploys(t, plain), w.deploys(t, qual))
	}
	_, err = w.orch.SetTierLock(ctx, w.org, "prod", false) // no change
	must(t, err)
	if w.deploys(t, qual) != 1 {
		t.Errorf("an unchanged lock redeployed")
	}
	w.setEnv(t, qual, "org.params.c[live].n")
	_, err = w.orch.RenameTier(ctx, w.org, "prod", "live")
	must(t, err)
	if w.deploys(t, qual) != 2 {
		t.Errorf("rename: qual %d, want 2 (a [new] ref)", w.deploys(t, qual))
	}
	must(t, w.orch.DeleteTier(ctx, w.org, "live"))
	if w.deploys(t, plain) != 2 {
		t.Errorf("last tier deleted: plain %d, want 2", w.deploys(t, plain))
	}
}

// Codex round 2: deleting a tier that is not the last still redeploys its
// [x] readers, and an env renamed from one tier to another redeploys.
func TestTierDeleteAndMoveRedeploy(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tl := w.tile(t, "plain", true) // in dev
	qual := w.tile(t, "qual", true)
	w.setEnv(t, qual, "org.params.c[stage].n")
	for _, s := range []string{"dev", "prod", "stage"} {
		_, err := w.orch.CreateTier(ctx, w.org, s)
		must(t, err)
	}
	_, err := w.orch.SetTierLock(ctx, w.org, "stage", false)
	must(t, err)
	q := w.deploys(t, qual)
	must(t, w.orch.DeleteTier(ctx, w.org, "stage"))
	if w.deploys(t, qual) != q+1 {
		t.Errorf("tier deleted: qual %d, want %d", w.deploys(t, qual), q+1)
	}
	n := w.deploys(t, tl)
	_, err = w.orch.RenameEnv(ctx, w.env, "prod")
	must(t, err)
	if w.deploys(t, tl) != n+1 {
		t.Errorf("dev renamed to prod: %d deploys, want %d", w.deploys(t, tl), n+1)
	}
}

// A tier made over a live env's slug redeploys that env's running tiles (they
// gain the tier block), even when the org already has tiers.
func TestCreateTierRedeploysLiveEnv(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tl := w.tile(t, "plain", true) // in dev
	_, err := w.orch.CreateTier(ctx, w.org, "prod")
	must(t, err)
	if w.deploys(t, tl) != 0 {
		t.Fatalf("an unrelated tier redeployed dev: %d", w.deploys(t, tl))
	}
	_, err = w.orch.CreateTier(ctx, w.org, "dev")
	must(t, err)
	if w.deploys(t, tl) != 1 {
		t.Errorf("tier over dev: %d deploys, want 1", w.deploys(t, tl))
	}
}

// Org-scope writes are refused once the org has tiers.
func TestOrgParamsRefusedWhenTiered(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	org := ParamScope{Kind: "org", ID: w.org}
	e := []ParamEntry{{Collection: "c", Name: "n", Kind: "param", Value: "1"}}
	_, err := w.orch.SetParams(ctx, org, e)
	must(t, err)
	_, err = w.orch.CreateTier(ctx, w.org, "dev")
	must(t, err)
	if _, err := w.orch.SetParams(ctx, org, e); !isConflict(err) || !strings.Contains(err.Error(), "this org has tiers; use --tier or --pr") {
		t.Errorf("set = %v", err)
	}
	if _, err := w.orch.DeleteParam(ctx, org, "c", "n"); !isConflict(err) {
		t.Errorf("delete = %v", err)
	}
}

// A template tile restarts for a change in a block it can read: its own, or
// an unlocked one; not a locked one elsewhere.
func TestTemplateReadersNeedReadableScope(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tp := w.tile(t, "tpl", true)
	_, err := w.st.DB().ExecContext(ctx, `UPDATE tiles SET files = 'a.yml:/a.yml:template' WHERE id = ?`, tp.ID)
	must(t, err)
	prod := uuid.NewString()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: prod, StackID: w.stack, Name: "prod", Slug: "prod", Type: "static", Settings: "{}", Network: "n2",
		Locked: true, FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	count := func(env string) int {
		s := ParamScope{Kind: "env", ID: env}
		ts, err := w.orch.scopeTiles(ctx, s)
		must(t, err)
		rs, err := w.orch.readers(ctx, s, ts, map[string]bool{"c.n": true}, false)
		must(t, err)
		return len(rs)
	}
	if count(w.env) != 1 {
		t.Error("own block: template not a reader")
	}
	if count(prod) != 0 {
		t.Error("locked block elsewhere: template is a reader")
	}
	_, err = w.orch.SetEnvLock(ctx, prod, false)
	must(t, err)
	if count(prod) != 1 {
		t.Error("unlocked block elsewhere: template not a reader")
	}
}

// Tier verbs: a delete names the stack env still in the tier, an env lock
// is refused on a tiered env and allowed off-tier, the tier scopes take
// params and redeploy nothing yet.
func TestTiers(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	dev, err := w.orch.CreateTier(ctx, w.org, "dev")
	must(t, err)
	_, err = w.orch.CreateTier(ctx, w.org, "prod")
	must(t, err)
	must(t, w.orch.ReorderTiers(ctx, w.org, []string{"prod", "dev"}))
	if ts, _ := w.orch.Tiers(ctx, w.org); len(ts) != 2 || ts[0].Slug != "prod" {
		t.Fatalf("tiers = %+v", ts)
	}
	if err := w.orch.ReorderTiers(ctx, w.org, []string{"prod"}); err == nil {
		t.Error("a partial order is accepted")
	}

	// w.env is "dev": in the tier
	if err := w.orch.DeleteTier(ctx, w.org, "dev"); !isConflict(err) || !strings.Contains(err.Error(), "shop/dev") {
		t.Errorf("delete = %v, want a conflict naming shop/dev", err)
	}
	if _, err := w.orch.SetEnvLock(ctx, w.env, false); !isConflict(err) || !strings.Contains(err.Error(), "dev's lock is the org's tier lock") {
		t.Errorf("env lock on a tiered env = %v", err)
	}
	if dev, err = w.orch.SetTierLock(ctx, w.org, "dev", false); err != nil || dev.Locked {
		t.Fatalf("unlock = %+v, %v", dev, err)
	}

	// a rename is refused while the env carries the slug; once the env moves
	// off, the tier renames: the env is off-tier, its own lock works, the
	// renamed tier deletes
	if _, err := w.orch.RenameTier(ctx, w.org, "dev", "develop"); !isConflict(err) || !strings.Contains(err.Error(), "shop/dev is still in tier dev; rename the env first") {
		t.Errorf("rename = %v, want a conflict naming shop/dev", err)
	}
	_, err = w.orch.RenameEnv(ctx, w.env, "staging")
	must(t, err)
	_, err = w.orch.RenameTier(ctx, w.org, "dev", "develop")
	must(t, err)
	e, err := w.orch.SetEnvLock(ctx, w.env, false)
	if err != nil || e.Locked {
		t.Fatalf("off-tier lock = %+v, %v", e, err)
	}
	must(t, w.orch.DeleteTier(ctx, w.org, "develop"))
	if _, err := w.orch.SetTierLock(ctx, w.org, "develop", true); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("lock a deleted tier = %v", err)
	}

	// params take the tier scopes
	ts, err := w.orch.Tiers(ctx, w.org)
	must(t, err)
	p := ts[0]
	for _, s := range []ParamScope{{Kind: "tier", ID: p.ID}, {Kind: "stack_pr", ID: w.stack}, {Kind: "org_pr", ID: w.org}} {
		if _, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "db", Name: "pass", Kind: "secret", Value: "x"}}); err != nil {
			t.Fatalf("%s: %v", s.Kind, err)
		}
		if ps, err := w.orch.MaskedParams(ctx, s); err != nil || len(ps) != 1 || ps[0].Value != "" {
			t.Errorf("%s masked = %+v, %v", s.Kind, ps, err)
		}
		if _, err := w.orch.DeleteParam(ctx, s, "db", "pass"); err != nil {
			t.Errorf("%s delete: %v", s.Kind, err)
		}
	}
}

// A PR env has no lock to set.
func TestSetEnvLockRefusesPREnv(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: "pr9", StackID: w.stack, Name: "pr-9", Slug: "pr-9", Type: "ephemeral", Settings: "{}",
		Network: "n9", FromKind: "branch", FromBranch: "feat", CreatedAt: time.Now(),
	}))
	if _, err := w.orch.SetEnvLock(ctx, "pr9", true); !isConflict(err) || !strings.Contains(err.Error(), "PR envs have no lock") {
		t.Errorf("lock on a PR env = %v", err)
	}
}
