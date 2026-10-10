package promote

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const paramsFile = `version: 1
stack: shop
ladder: [dev, prd]
head: main
params:
  app:
    dev:
      mode: fast
      key: {type: secret, generate: 24}
      sec: {type: secret}
    pr:
      mode: sandbox
base:
  tiles:
    api: {image: nginx:1, port: 80}
environments:
  dev: {locked: true}
  prd: {from: promote}
`

func rows(p *Plan, kind string) map[string]Change {
	out := map[string]Change{}
	for _, c := range p.Changes {
		if c.Kind == kind {
			out[c.Field+c.Tile] = c
		}
	}
	return out
}

// Per-env params: add, generate, drift, becomes-secret; a secret never drifts.
func TestPlanEnvParams(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.files["p1"] = paramsFile
	r := w.release(t, "p1")
	scope := params.Scope{Kind: "env", ID: w.dev.ID}

	p := planOK(t, w, w.dev, r)
	got := rows(p, "param")
	if got["app.mode"].New != "fast" || got["app.mode"].Old != "" || got["app.key"].Note != "generated" ||
		got["pr.app.mode"].New != "sandbox" || len(got) != 3 {
		t.Errorf("first plan = %+v", got)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "app.sec") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	_, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if len(rows(planOK(t, w, w.dev, r), "param")) != 0 {
		t.Error("a second plan still has param rows")
	}

	// A panel edit is drift: both values on the row, applying puts the file's back.
	must(t, w.f.D.Params.Set(ctx, scope, params.Entry{Collection: "app", Name: "mode", Kind: params.Param, Value: "slow"}))
	must(t, w.f.D.Params.Set(ctx, scope, params.Entry{Collection: "app", Name: "key", Kind: params.Secret, Value: "other"}))
	must(t, w.f.D.Params.Set(ctx, scope, params.Entry{Collection: "app", Name: "sec", Kind: params.Param, Value: "plain"}))
	got = rows(planOK(t, w, w.dev, r), "param")
	m := got["app.mode"]
	if m.Old != "slow" || m.New != "fast" || !strings.Contains(m.Note, "panel has slow, file says fast") ||
		got["app.sec"].Note != "becomes a secret" || len(got) != 2 {
		t.Errorf("drift plan = %+v", got)
	}
	_, err = w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	vals, err := w.f.D.Params.Values(ctx, scope, true)
	must(t, err)
	if vals["app.mode"].V != "fast" || vals["app.key"].V != "other" || !vals["app.sec"].Secret {
		t.Errorf("after apply = %+v", vals)
	}
}

// A file lock that differs from live is a row that does not apply; a tiered
// env's lock is its tier's.
func TestPlanEnvLock(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.files["p1"] = paramsFile
	r := w.release(t, "p1")
	e, err := w.f.D.Envs.SetRelease(ctx, w.dev, r.ID)
	must(t, err)
	e, err = w.f.D.Envs.SetLocked(ctx, e, false)
	must(t, err)

	c := rows(planOK(t, w, e, r), "lock")["dev"]
	if c.Old != "unlocked" || c.New != "locked" || !strings.Contains(c.Note, "member") {
		t.Fatalf("lock row = %+v", c)
	}
	_, err = w.f.Apply(ctx, e.ID, r.ID, io.Discard, nil)
	must(t, err)
	if e, err = w.f.D.Envs.Get(ctx, e.ID); err != nil || e.Locked {
		t.Errorf("the plan applied the lock: %v %v", e.Locked, err)
	}
	if len(rows(planOK(t, w, e, r), "lock")) != 1 {
		t.Error("the lock row went away without the button")
	}

	_, err = tier.New(w.s.Tiers).Create(ctx, w.st.OrgID, "dev")
	must(t, err)
	p := planOK(t, w, e, r)
	if len(rows(p, "lock")) != 0 || !strings.Contains(strings.Join(p.Warnings, "|"), "environments.dev.locked is ignored") {
		t.Errorf("tiered env: rows %v, warnings %v", rows(p, "lock"), p.Warnings)
	}
}

const prFile = `version: 1
stack: shop
ladder: [dev, prd]
head: main
params:
  app:
    pr:
      mode: sandbox
      tok: {type: secret}
base:
  tiles:
    api: {image: nginx:1, port: 80}
environments:
  dev: {locked: true}
  prd: {from: promote}
`

func (w *world) prEnv(t *testing.T, name string) store.Environment {
	t.Helper()
	base := w.dev.ID
	pr := store.Environment{
		ID: uuid.NewString(), StackID: w.st.ID, Name: name, Slug: name, Type: environment.Ephemeral,
		BaseEnvID: &base, Settings: "{}", Network: "n", Position: 2, FromKind: environment.FromBranch,
		FromBranch: "feature", CreatedAt: time.Now(),
	}
	must(t, w.s.Environments.Create(ctx, pr))
	return pr
}

func (w *world) vals(t *testing.T, kind, id string) map[string]params.Value {
	t.Helper()
	v, err := w.f.D.Params.Values(ctx, params.Scope{Kind: kind, ID: id}, true)
	must(t, err)
	return v
}

// D1: a PR env keeps its own params. The plan diffs the file's pr block
// against the env's own scope and writes plain values there only; a secret
// only declares (a missing one warns, none is ever written) and stack_pr is
// left alone.
func TestPlanPRParams(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.files["p1"] = prFile
	r := w.release(t, "p1")
	pr := w.prEnv(t, "pr-3")

	p := planOK(t, w, pr, r)
	got := rows(p, "param")
	if got["app.mode"].New != "sandbox" || len(got) != 1 {
		t.Fatalf("pr plan = %+v", got)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "app.tok") {
		t.Errorf("warnings = %v", p.Warnings)
	}
	_, err := w.f.Apply(ctx, pr.ID, r.ID, io.Discard, nil)
	must(t, err)
	own, shared := w.vals(t, "env", pr.ID), w.vals(t, "stack_pr", w.st.ID)
	if own["app.mode"].V != "sandbox" || len(own) != 1 || len(shared) != 0 {
		t.Errorf("env %+v, stack_pr %+v", own, shared)
	}
	if len(rows(planOK(t, w, pr, r), "param")) != 0 {
		t.Error("a second pr plan still has param rows")
	}

	// The seeded secret stays; a branch that makes it plain is blocked, and one
	// that drops the declaration leaves it as an optional removal row.
	must(t, w.f.D.Params.Set(ctx, params.Scope{Kind: "env", ID: pr.ID}, params.Entry{Collection: "app", Name: "tok", Kind: params.Secret, Value: "seeded"}))
	if got := rows(planOK(t, w, pr, r), "param"); len(got) != 0 {
		t.Errorf("a seeded secret the branch declares moved: %+v", got)
	}
	w.files["p2"] = strings.Replace(prFile, "      tok: {type: secret}\n", "", 1)
	r2 := w.release(t, "p2")
	c := rows(planOK(t, w, pr, r2), "param")["app.tok"]
	if !c.Optional || c.Key != "param:app.tok" {
		t.Errorf("dropped secret row = %+v", c)
	}
	_, err = w.f.Apply(ctx, pr.ID, r2.ID, io.Discard, nil)
	must(t, err)
	if _, ok := w.vals(t, "env", pr.ID)["app.tok"]; !ok {
		t.Error("an unticked secret removal deleted the secret")
	}
}

// D2: a name the file no longer sets is removed: plain ones with the plan,
// secrets only when ticked. A static env's promote also writes the pr block
// to stack_pr and removes its strays.
func TestPlanParamRemovals(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.files["p1"] = paramsFile
	r := w.release(t, "p1")
	env := params.Scope{Kind: "env", ID: w.dev.ID}
	shared := params.Scope{Kind: "stack_pr", ID: w.st.ID}
	set := func(sc params.Scope, n, v, k string) {
		must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "app", Name: n, Kind: k, Value: v}))
	}
	set(env, "stray", "x", params.Param)
	set(env, "oldpw", "pw", params.Secret)
	set(shared, "stray", "y", params.Param)
	set(shared, "oldpw", "pw", params.Secret)

	p := planOK(t, w, w.dev, r)
	got := rows(p, "param")
	if c := got["app.stray"]; c.Old != "x" || c.Optional || !strings.Contains(c.Note, "removed") {
		t.Errorf("plain removal = %+v", c)
	}
	if c := got["pr.app.stray"]; c.Old != "y" || c.Optional {
		t.Errorf("pr plain removal = %+v", c)
	}
	if c := got["app.oldpw"]; !c.Optional || c.Key != "param:app.oldpw" {
		t.Errorf("secret removal = %+v", c)
	}
	if c := got["pr.app.oldpw"]; !c.Optional || c.Key != "param:pr.app.oldpw" {
		t.Errorf("pr secret removal = %+v", c)
	}

	_, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	for _, v := range []map[string]params.Value{w.vals(t, "env", w.dev.ID), w.vals(t, "stack_pr", w.st.ID)} {
		if _, ok := v["app.stray"]; ok {
			t.Errorf("plain removal did not apply: %+v", v)
		}
		if _, ok := v["app.oldpw"]; !ok {
			t.Errorf("unticked secret removal applied: %+v", v)
		}
	}
	_, err = w.f.ApplyTicked(ctx, w.dev.ID, r.ID, []string{"param:app.oldpw"}, io.Discard, nil)
	must(t, err)
	if _, ok := w.vals(t, "env", w.dev.ID)["app.oldpw"]; ok {
		t.Error("a ticked secret removal did not apply")
	}
	if _, ok := w.vals(t, "stack_pr", w.st.ID)["app.oldpw"]; !ok {
		t.Error("a tick for the env row deleted the pr secret")
	}
}

// Promote and sync write params through WriteParams, naming the env they roll
// out so only readers elsewhere redeploy.
func TestPromoteWritesThroughHook(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	w.files["p1"] = paramsFile
	r := w.release(t, "p1")
	var scopes []string
	w.f.WriteParams = func(ctx context.Context, s params.Scope, skip string, write func() error) error {
		if skip != w.dev.ID {
			t.Errorf("skip env = %q", skip)
		}
		scopes = append(scopes, s.Kind)
		return write()
	}
	_, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
	must(t, err)
	if strings.Join(scopes, " ") != "env stack_pr" {
		t.Errorf("hook scopes = %v", scopes)
	}
}

// Nothing syncs into a PR env.
func TestSyncIntoPREnvRefused(t *testing.T) {
	w := setup(t)
	pr := w.prEnv(t, "pr-4")
	_, err := w.f.SyncPlan(ctx, pr.ID, w.dev.ID, nil)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "PR env") {
		t.Errorf("sync plan into a PR env: %v", err)
	}
	_, err = w.f.SyncApply(ctx, pr.ID, w.dev.ID, nil, "x", io.Discard, nil)
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("sync apply into a PR env: %v", err)
	}
}

// Export merges identical env blocks into a|b keys, carries the pr block and
// the lock, and plans clean.
func TestExportParamsShape(t *testing.T) {
	w := setup(t)
	w.fake.Digests = map[string]string{"nginx:1": "sha256:one"}
	set := func(sc params.Scope, n, v string, k string) {
		must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "app", Name: n, Kind: k, Value: v}))
	}
	for _, e := range []store.Environment{w.dev, w.prd} {
		set(params.Scope{Kind: "env", ID: e.ID}, "host", "h", params.Param)
		set(params.Scope{Kind: "env", ID: e.ID}, "pw", "s3cret-"+e.Slug, params.Secret)
	}
	set(params.Scope{Kind: "env", ID: w.prd.ID}, "extra", "x", params.Param)
	set(params.Scope{Kind: "stack_pr", ID: w.st.ID}, "host", "sandbox", params.Param)
	out, _, err := w.f.Export(ctx, w.st.ID, "")
	must(t, err)
	got := string(out)
	for _, want := range []string{"    dev:\n      host: h", "    prd:\n      extra: x", "    pr:\n      host: sandbox", "type: secret"} {
		if !strings.Contains(got, want) {
			t.Errorf("export lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "locked:") {
		t.Errorf("export:\n%s", got)
	}
	r, err := Load(out, nil, "acme")
	must(t, err)
	if r.Params["pr"]["app.host"].Value != "sandbox" || !r.Params["dev"]["app.pw"].Secret {
		t.Errorf("reload = %+v", r.Params)
	}
	w.files["c1"] = got
	rel := w.release(t, "c1")
	for _, e := range []store.Environment{w.dev, w.prd} {
		_, err = w.f.D.Envs.SetRelease(ctx, e, rel.ID)
		must(t, err)
		if p := planOK(t, w, e, rel); len(p.Changes) != 0 {
			t.Errorf("plan of %s = %s\n%s", e.Slug, kinds(p), got)
		}
	}

	// Dropping prd's extra leaves two identical blocks, still one key.
	must(t, w.f.D.Params.Delete(ctx, params.Scope{Kind: "env", ID: w.prd.ID}, "app", "extra"))
	out, _, err = w.f.Export(ctx, w.st.ID, "")
	must(t, err)
	if !strings.Contains(string(out), "    dev|prd:\n      host: h") {
		t.Errorf("no merged key:\n%s", out)
	}
}
