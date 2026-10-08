package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Shell opens in the running replica, not the first listed; a system
// container is refused; no replica is a conflict.
func TestShell(t *testing.T) {
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	ctx := context.Background()
	lbl := map[string]string{"stackr.tile": tl.ID, "stackr.role": "replica"}

	if _, _, _, err := env.Orch.Shell(ctx, "u", tl.ID, "", ""); !errors.As(err, new(errs.Conflict)) {
		t.Errorf("no replica = %v, want a conflict", err)
	}

	env.Docker.Containers = []docker.Container{
		{ID: "c-old", State: "exited", Labels: lbl},
		{ID: "c-live", State: "running", Labels: lbl},
		{ID: "panel", State: "running", Labels: map[string]string{"stackr.system": "panel"}},
	}
	conn, _, finish, err := env.Orch.Shell(ctx, "u", tl.ID, "", "zsh")
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	_ = conn
	last := env.Docker.Calls()[len(env.Docker.Calls())-1]
	if last.String() != "ExecTTY(c-live, zsh)" {
		t.Errorf("exec = %s, want zsh in the running replica", last)
	}

	_, _, _, err = env.Orch.Shell(ctx, "u", tl.ID, "panel", "")
	if !errors.As(err, new(errs.Conflict)) || !strings.Contains(err.Error(), "cannot be") {
		t.Errorf("system container = %v, want refused", err)
	}
}
