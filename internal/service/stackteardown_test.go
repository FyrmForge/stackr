package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// A teardown wants the slug typed, then removes the tiles, the envs and the
// stack in one job.
func TestTeardownStack(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.tile(t, "web", true)
	w.tile(t, "api", false)
	w.newEnv(t, "prod", "static")

	if _, err := w.orch.TeardownStack(ctx, w.stack, "nope"); !isInvalid(err) {
		t.Fatalf("wrong confirm = %v, want invalid", err)
	}
	j, err := w.orch.TeardownStack(ctx, w.stack, "shop")
	must(t, err)
	if got := w.wait(t, j.ID); got.State != "done" {
		t.Fatalf("teardown = %s %s", got.State, got.Error)
	}
	if _, err := w.st.Stacks.Get(ctx, w.stack); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("stack still there: %v", err)
	}
	if es, _ := w.st.Environments.ListByStack(ctx, w.stack); len(es) != 0 {
		t.Errorf("envs left: %d", len(es))
	}
	if ts, _ := w.st.Tiles.ListByStack(ctx, w.stack); len(ts) != 0 {
		t.Errorf("tiles left: %d", len(ts))
	}
}

func isInvalid(err error) bool {
	_, ok := errs.IsInvalid(err)
	return ok
}
