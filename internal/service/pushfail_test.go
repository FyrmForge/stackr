package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A stack's push failure stands until a later push does not fail; a
// superseded push in between says nothing.
func TestPushFailure(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	push := func(state, msg string) string {
		at = at.Add(time.Minute)
		id := uuid.NewString()
		if err := env.Store.Jobs.Create(ctx, store.Job{ID: id, Kind: "push", State: state, Error: msg,
			LockSet: store.StringList{"push:s1"}, Payload: `{}`, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, ok := env.Orch.PushFailure(ctx, "s1"); ok {
		t.Fatal("a stack with no push has no failure")
	}
	bad := push("failed", "buildx build: exit status 1")
	push("superseded", "")
	if f, ok := env.Orch.PushFailure(ctx, "s1"); !ok || f.JobID != bad || f.Error != "buildx build: exit status 1" {
		t.Errorf("failure = %+v, %v", f, ok)
	}
	if _, ok := env.Orch.PushFailure(ctx, "s2"); ok {
		t.Error("another stack's failure leaks")
	}
	push("running", "")
	if _, ok := env.Orch.PushFailure(ctx, "s1"); ok {
		t.Error("a retry under way still shows the failure")
	}
}
