package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A branch's push failure stands until a later push of that same branch
// lands: superseded, cancelled, queued and running pushes say nothing, nor
// does another branch's push; any failing branch shows, however many pushes
// came after.
func TestPushFailure(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	push := func(stack, branch, state, msg string) string {
		at = at.Add(time.Minute)
		id := uuid.NewString()
		if err := env.Store.Jobs.Create(ctx, store.Job{ID: id, Kind: "push", State: state, Error: msg,
			LockSet: store.StringList{"push:" + stack, "push:" + stack + ":acme/app@" + branch}, Payload: `{}`, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, ok := env.Orch.PushFailure(ctx, "s1"); ok {
		t.Fatal("a stack with no push has no failure")
	}
	bad := push("s1", "main", "failed", "buildx build: exit status 1")
	push("s1", "main", "superseded", "")
	push("s1", "main", "cancelled", "")
	if f, ok := env.Orch.PushFailure(ctx, "s1"); !ok || f.JobID != bad || f.Error != "buildx build: exit status 1" {
		t.Errorf("failure = %+v, %v", f, ok)
	}
	if _, ok := env.Orch.PushFailure(ctx, "s2"); ok {
		t.Error("another stack's failure leaks")
	}
	push("s1", "main", "running", "")
	push("s1", "main", "queued", "")
	if _, ok := env.Orch.PushFailure(ctx, "s1"); !ok {
		t.Error("a retry under way hides the failure")
	}
	for i := 0; i < 15; i++ {
		push("s1", "dev", "done", "")
	}
	if _, ok := env.Orch.PushFailure(ctx, "s1"); !ok {
		t.Error("another branch's pushes clear the failure")
	}
	push("s1", "main", "done", "")
	if _, ok := env.Orch.PushFailure(ctx, "s1"); ok {
		t.Error("a landed push of the same branch keeps the failure")
	}
	other := push("s1", "dev", "failed", "denied")
	if f, ok := env.Orch.PushFailure(ctx, "s1"); !ok || f.JobID != other {
		t.Errorf("failing branch = %+v, %v", f, ok)
	}
}
