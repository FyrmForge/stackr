package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// envTile makes a running image tile in env that reads ref.
func (w *world) envTile(t *testing.T, env, name, ref string) Tile {
	t.Helper()
	ctx := context.Background()
	tl, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: env, Name: name,
		Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	_, err = w.st.DB().ExecContext(ctx, `UPDATE tiles SET env_json = ? WHERE id = ?`, `{"V":"${{ `+ref+` }}"}`, tl.ID)
	must(t, err)
	w.fake.Containers = append(w.fake.Containers, docker.Container{
		ID: "c-" + name, Name: "c-" + name, State: "running",
		Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
	})
	return tl
}

func (w *world) newEnv(t *testing.T, slug, typ string) string {
	t.Helper()
	id := uuid.NewString()
	must(t, w.st.Environments.Create(context.Background(), store.Environment{
		ID: id, StackID: w.stack, Name: slug, Slug: slug, Type: typ, Settings: "{}", Network: "n-" + slug,
		FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	return id
}

func (w *world) val(t *testing.T, s ParamScope) map[string]string {
	t.Helper()
	vs, err := w.orch.params.Values(context.Background(), s, true)
	must(t, err)
	out := map[string]string{}
	for k, v := range vs {
		out[k] = v.V
		if v.Secret {
			out[k] = "secret:" + v.V
		}
	}
	return out
}

func names(rs []Redeploy) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Env+"/"+r.Tile)
	}
	return strings.Join(out, " ")
}

// D1: a new PR env copies the stack's pr block, secrets included; a changed
// or deleted pr secret reaches every open PR env and redeploys its readers,
// a plain change does not.
func TestPREnvParamsCopy(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	shared := ParamScope{Kind: "stack_pr", ID: w.stack}
	_, err := w.orch.SetParams(ctx, shared, []ParamEntry{
		{Collection: "c", Name: "mode", Kind: "param", Value: "sandbox"},
		{Collection: "c", Name: "tok", Kind: "secret", Value: "t1"},
	})
	must(t, err)
	pr := w.newEnv(t, "pr-1", "ephemeral")
	env, err := w.orch.envs.Get(ctx, pr)
	must(t, err)
	must(t, w.orch.seedPREnv(ctx, env))
	own := ParamScope{Kind: "env", ID: pr}
	if got := w.val(t, own); got["c.mode"] != "sandbox" || got["c.tok"] != "secret:t1" || len(got) != 2 {
		t.Fatalf("seeded copy = %v", got)
	}

	w.envTile(t, pr, "web", "params.c.tok")
	w.envTile(t, pr, "side", "params.c.mode")
	rs, err := w.orch.SetParams(ctx, shared, []ParamEntry{
		{Collection: "c", Name: "mode", Kind: "param", Value: "other"},
		{Collection: "c", Name: "tok", Kind: "secret", Value: "t2"},
	})
	must(t, err)
	if got := w.val(t, own); got["c.mode"] != "sandbox" || got["c.tok"] != "secret:t2" {
		t.Errorf("after the pr secret changed: %v", got)
	}
	if names(rs) != "pr-1/web" {
		t.Errorf("redeploys = %q, want only the reader of the secret", names(rs))
	}

	if _, err := w.orch.DeleteParam(ctx, shared, "c", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.val(t, own)["c.tok"]; ok {
		t.Error("a pr secret deleted from the template stayed in the PR env")
	}
}

// Item 12: a promote or sync write (changeParamsFrom with the rolled-out env
// skipped) redeploys readers in other envs and leaves that env to its job.
func TestPromoteParamWriteRedeploysOtherEnvs(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	prod := w.newEnv(t, "prod", "static")
	dev := ParamScope{Kind: "env", ID: w.env}
	w.envTile(t, w.env, "own", "params.c.n")
	w.envTile(t, prod, "far", "params.c[dev].n")
	rs, err := w.orch.changeParamsFrom(ctx, dev, w.env, func() error {
		return w.orch.params.Set(ctx, dev, ParamEntry{Collection: "c", Name: "n", Kind: "param", Value: "v"})
	})
	must(t, err)
	if names(rs) != "prod/far" {
		t.Errorf("redeploys = %q, want prod/far only", names(rs))
	}
	if err := w.orch.writeParams(ctx, dev, w.env, func() error {
		return w.orch.params.Set(ctx, dev, ParamEntry{Collection: "c", Name: "n", Kind: "param", Value: "w"})
	}); err != nil {
		t.Fatal(err)
	}
}
