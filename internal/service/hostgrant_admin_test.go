package service

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A server admin's ask is granted on the spot, so the park's text says it was
// approved; any other actor's still says it waits.
func TestParkOnApprovalSaysAdminApproved(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	must(t, w.st.Users.Create(ctx, store.User{ID: "dev", Email: "d@b.c", Name: "D", Role: "user", Active: true, CreatedAt: now, UpdatedAt: now}))
	n := errs.NeedsApproval{Stack: w.stack, What: "elevated access: mon published:8080:80"}
	for actor, want := range map[string]string{"adm": "approved by you", "dev": "waiting:"} {
		r := &jobs.Run{Job: store.Job{ActorID: actor, Payload: "{}"}}
		err := w.orch.parkOnApproval(ctx, r, n)
		if _, ok := errs.IsNeedsApproval(err); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to hold %q", actor, err, want)
		}
	}
}

// A tile a promote deploys joins the job's lock set, so the tile's
// lock set (the runner writes it; its own test covers that); a tile it never reached does not.
func TestLockLogListsTheJobForADeployedTileOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	j := store.Job{ID: "pj", Kind: string(kindPromote), State: "running", LockSet: store.StringList{"env:" + w.env}, Payload: "{}", CreatedAt: time.Now()}
	must(t, w.st.Jobs.Create(ctx, j))
	id := w.tile(t, "api", false).ID
	other := w.tile(t, "web", false).ID
	r := &jobs.Run{Job: j, Log: io.Discard}
	_, _ = lockLog{ctx, w.orch, r, w.env}.Write([]byte("deploying api\n"))
	if !slices.Contains(r.Job.LockSet, id) || slices.Contains(r.Job.LockSet, other) {
		t.Fatalf("lock set = %v, want only the deployed tile %s", r.Job.LockSet, id)
	}
}
