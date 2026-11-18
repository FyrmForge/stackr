package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
)

// RevealParam returns the one secret asked for, logs its name and never its
// value, and refuses a param or a missing row.
func TestRevealParam(t *testing.T) {
	t.Parallel()
	w, adm := secWorld(t)
	ctx := context.Background()
	scope := ParamScope{Kind: "org", ID: w.org}
	_, err := w.orch.SetParams(ctx, scope, []ParamEntry{
		{Collection: "app", Name: "token", Kind: "secret", Value: "s3cret"},
		{Collection: "app", Name: "other", Kind: "secret", Value: "n0pe"},
		{Collection: "app", Name: "mode", Kind: "param", Value: "fast"},
	})
	must(t, err)
	got, err := w.orch.RevealParam(ctx, adm, scope, "app", "token")
	must(t, err)
	if got != "s3cret" {
		t.Errorf("value = %q", got)
	}
	e := one(t, w, securitylog.SecretReveal)
	if e.Detail != "app.token" || strings.Contains(e.Detail, "s3cret") || strings.Contains(e.Detail, "n0pe") {
		t.Errorf("log = %+v", e)
	}
	for _, name := range []string{"mode", "missing"} {
		if _, err := w.orch.RevealParam(ctx, adm, scope, "app", name); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("reveal %s = %v, want not found", name, err)
		}
	}
	if n := len(events(t, w, securitylog.SecretReveal)); n != 1 {
		t.Errorf("%d reveal rows, want 1: a refusal logs nothing", n)
	}
}

// ParamImpact lists the running readers of what changed and writes nothing.
func TestParamImpact(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	_, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "a"}})
	must(t, err)
	w.envTile(t, w.env, "web", "params.c.mode")
	w.envTile(t, w.env, "side", "params.c.other")
	same := []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "a"}}
	if got, err := w.orch.ParamImpact(ctx, s, same); err != nil || len(got) != 0 {
		t.Errorf("unchanged = %v %v", got, err)
	}
	diff := []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "b"}}
	if got, err := w.orch.ParamImpact(ctx, s, diff); err != nil || !slices.Equal(got, []string{"web"}) {
		t.Errorf("changed = %v %v", got, err)
	}
	if got := w.val(t, s); got["c.mode"] != "a" {
		t.Errorf("preview wrote: %v", got)
	}
}

// A tile parked on a missing value shows as unset with the tile that waits; a
// restart-later save of it cancels the parked deploy and marks nothing for a
// tile that was not running.
func TestUnsetParams(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	tl, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: w.env, Name: "bazarr", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	_, err = w.st.DB().ExecContext(ctx, `UPDATE tiles SET env_json = ? WHERE id = ?`, `{"P":"${{ params.gate.password }}"}`, tl.ID)
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
	}
	got, err := w.orch.UnsetParams(ctx, s)
	must(t, err)
	if !slices.Equal(got["gate.password"], []string{"bazarr"}) {
		t.Fatalf("unset = %v", got)
	}
	names, err := w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "gate", Name: "password", Kind: "secret", Value: "pw"}})
	must(t, err)
	if len(names) != 0 || w.orch.RestartPending(tl.ID) {
		t.Errorf("a stopped tile is marked: %v", names)
	}
	if j = w.wait(t, j.ID); j.State != job.Cancelled {
		t.Errorf("parked deploy = %s, want cancelled so it does not start", j.State)
	}
	if got, _ := w.orch.UnsetParams(ctx, s); len(got) != 0 {
		t.Errorf("still unset: %v", got)
	}
}

// Redeploy later saves without redeploying, marks the running readers, and the
// mark clears when the tile is deployed.
func TestSetParamsLater(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	_, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "a"}})
	must(t, err)
	tl := w.envTile(t, w.env, "web", "params.c.mode")
	names, err := w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "b"}})
	must(t, err)
	if !slices.Equal(names, []string{"web"}) || !w.orch.RestartPending(tl.ID) {
		t.Fatalf("names = %v pending = %v", names, w.orch.RestartPending(tl.ID))
	}
	if got := w.val(t, s); got["c.mode"] != "b" {
		t.Errorf("not saved: %v", got)
	}
	if js, _ := w.orch.Jobs(ctx, 0); len(js) != 0 {
		for _, j := range js {
			if j.Kind == "deploy" {
				t.Errorf("a deploy was queued: %+v", j)
			}
		}
	}
	dj, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	// the mark follows the deploy's outcome, not its enqueue
	if dj = w.wait(t, dj.ID); dj.State == job.Done && w.orch.RestartPending(tl.ID) {
		t.Error("the mark stayed after a deploy that succeeded")
	} else if dj.State != job.Done && !w.orch.RestartPending(tl.ID) {
		t.Errorf("the mark cleared though the deploy ended %s", dj.State)
	}
}
