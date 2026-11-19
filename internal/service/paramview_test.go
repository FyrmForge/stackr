package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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

// A secret typed again with the value it already has is no redeploy, as in the save.
func TestParamImpactSecretSame(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	_, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "token", Kind: "secret", Value: "one"}})
	must(t, err)
	w.envTile(t, w.env, "web", "params.c.token")
	if got, err := w.orch.ParamImpact(ctx, s, []ParamEntry{{Collection: "c", Name: "token", Kind: "secret", Value: "one"}}); err != nil || len(got) != 0 {
		t.Errorf("same secret = %v %v", got, err)
	}
	if got, err := w.orch.ParamImpact(ctx, s, []ParamEntry{{Collection: "c", Name: "token", Kind: "secret", Value: "two"}}); err != nil || !slices.Equal(got, []string{"web"}) {
		t.Errorf("new secret = %v %v", got, err)
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
	names, waiting, err := w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "gate", Name: "password", Kind: "secret", Value: "pw"}})
	must(t, err)
	if !slices.Equal(waiting, []string{"bazarr"}) || !w.orch.RestartPending(tl.ID) {
		t.Errorf("the tile whose deploy was cancelled is not named and marked: %v", waiting)
	}
	if len(names) != 0 {
		t.Errorf("a stopped tile redeploys: %v", names)
	}
	if j = w.wait(t, j.ID); j.State != job.Cancelled {
		t.Errorf("parked deploy = %s, want cancelled so it does not start", j.State)
	}
	if got, _ := w.orch.UnsetParams(ctx, s); len(got) != 0 {
		t.Errorf("still unset: %v", got)
	}
}

// A secret a running tile reads and the env has no value for is listed with no
// tile waiting, though no deploy is parked on it.
func TestUnsetParamsListsWhatARunningTileReads(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	w.envTile(t, w.env, "web", "params.c.token")
	got, err := w.orch.UnsetParams(ctx, s)
	must(t, err)
	if tiles, ok := got["c.token"]; !ok || len(tiles) != 0 {
		t.Fatalf("unset = %v, want c.token listed with no tile", got)
	}
	_, err = w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "token", Kind: "secret", Value: "x"}})
	must(t, err)
	if got, _ := w.orch.UnsetParams(ctx, s); len(got) != 0 {
		t.Errorf("still unset once set: %v", got)
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
	names, _, err := w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "b"}})
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

// Only a waiting row waits: a deploy back queued (resumed) is no longer listed
// as waiting for its value, and a waiting one is cancelled by a later save, at
// env scope and at org scope.
func TestOnlyWaitingRowsWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, ref string }{
		{"env", "gate.password"},
		{"org", "org.gate.password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			ctx := context.Background()
			es := ParamScope{Kind: "env", ID: w.env}
			tl, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: w.env, Name: "bazarr", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80})
			must(t, err)
			ref := "${{ params.gate.password }}"
			s := es
			if tc.name == "org" {
				ref, s = "${{ org.params.gate.password }}", ParamScope{Kind: "org", ID: w.org}
			}
			_, err = w.st.DB().ExecContext(ctx, `UPDATE tiles SET env_json = ? WHERE id = ?`, `{"P":"`+ref+`"}`, tl.ID)
			must(t, err)
			j, err := w.orch.Deploy(ctx, tl.ID)
			must(t, err)
			if j = w.waiting(t, j.ID); j.State != job.Waiting {
				t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
			}
			// the runner has resumed it: queued, its row without a param
			_, err = w.st.DB().ExecContext(ctx, `UPDATE jobs SET state = 'queued', waiting_param = NULL WHERE id = ?`, j.ID)
			must(t, err)
			if tc.name == "env" {
				if got, _ := w.orch.UnsetParams(ctx, es); len(got) != 0 {
					t.Errorf("a resumed job still waits: %v", got)
				}
			}
			_, err = w.st.DB().ExecContext(ctx, `UPDATE jobs SET state = 'waiting', waiting_param = ? WHERE id = ?`, tc.ref, j.ID)
			must(t, err)
			_, waiting, err := w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "gate", Name: "password", Kind: "secret", Value: "pw"}})
			must(t, err)
			if !slices.Equal(waiting, []string{"bazarr"}) {
				t.Errorf("waiting = %v", waiting)
			}
			if j = w.wait(t, j.ID); j.State != job.Cancelled {
				t.Errorf("deploy = %s, want cancelled", j.State)
			}
		})
	}
}

// Redeploy pending stays while a deploy fails and clears when one succeeds:
// the fake's container exits during the health check until it is told to run.
func TestRedeployPendingClearsOnSuccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	_, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "a"}})
	must(t, err)
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: false}}
	tl := w.tile(t, "web", true)
	_, err = w.st.DB().ExecContext(ctx, `UPDATE tiles SET env_json = ? WHERE id = ?`, `{"V":"${{ params.c.mode }}"}`, tl.ID)
	must(t, err)
	if _, _, err = w.orch.SetParamsLater(ctx, s, []ParamEntry{{Collection: "c", Name: "mode", Kind: "param", Value: "b"}}); err != nil {
		t.Fatal(err)
	}
	dj, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if dj = w.wait(t, dj.ID); dj.State != job.Failed || !w.orch.RestartPending(tl.ID) {
		t.Fatalf("failing deploy = %s, pending = %v; want failed and still pending", dj.State, w.orch.RestartPending(tl.ID))
	}
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	dj, err = w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if dj = w.wait(t, dj.ID); dj.State != job.Done {
		t.Fatalf("deploy = %s %q, want done", dj.State, dj.Error)
	}
	if w.orch.RestartPending(tl.ID) {
		t.Error("the mark stayed after a deploy that succeeded")
	}
}

// A param park resolves on its own value only: an env param for a plain
// ref, an org param for an org. ref; another param landing resolves nothing.
func TestParamResolvesReadsTheJobsOwnParam(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	j := Job{Payload: `{"env_id":"` + w.env + `"}`}
	for _, c := range []struct {
		param string
		scope ParamScope
	}{{"app.k", ParamScope{Kind: "env", ID: w.env}}, {"org.app.k", ParamScope{Kind: "org", ID: w.org}}} {
		_, err := w.orch.SetParams(ctx, ParamScope{Kind: "env", ID: w.env}, []ParamEntry{{Collection: "app", Name: "other", Kind: "param", Value: "x"}})
		must(t, err)
		if ok, err := w.orch.paramResolves(ctx, j, c.param); err != nil || ok {
			t.Fatalf("%s before it is set: %v, %v", c.param, ok, err)
		}
		_, err = w.orch.SetParams(ctx, c.scope, []ParamEntry{{Collection: "app", Name: "k", Kind: "param", Value: "v"}})
		must(t, err)
		if ok, err := w.orch.paramResolves(ctx, j, c.param); err != nil || !ok {
			t.Fatalf("%s after it is set: %v, %v", c.param, ok, err)
		}
	}
}

// A save that fails leaves the parked deploy alone: it is cancelled only
// after the value is written.
func TestFailedSaveKeepsParkedDeploy(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	s := ParamScope{Kind: "env", ID: w.env}
	_, err := w.orch.SetParams(ctx, s, []ParamEntry{{Collection: "c", Name: "key", Kind: "secret", Value: "x"}})
	must(t, err)
	tl, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: w.env, Name: "bazarr", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	_, err = w.st.DB().ExecContext(ctx, `UPDATE tiles SET env_json = ? WHERE id = ?`, `{"P":"${{ params.gate.password }}"}`, tl.ID)
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy = %s %q, want parked", j.State, j.Error)
	}
	// the second entry turns a secret into a param: the whole save is refused
	_, _, err = w.orch.SetParamsLater(ctx, s, []ParamEntry{
		{Collection: "gate", Name: "password", Kind: "secret", Value: "pw"},
		{Collection: "c", Name: "key", Kind: "param", Value: "y"},
	})
	if err == nil {
		t.Fatal("the save was accepted")
	}
	if got, err := w.orch.jobs.Get(ctx, j.ID); err != nil || got.State != job.Waiting {
		t.Fatalf("deploy = %v %v, want still waiting", got.State, err)
	}
}
