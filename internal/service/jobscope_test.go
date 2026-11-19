package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/google/uuid"
)

// The jobs lists name a job's stack, and the env when the payload names one;
// a job about no stack has no scope.
func TestJobScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	e, err := w.st.Environments.Get(ctx, w.env)
	must(t, err)
	add := func(kind, payload string) string {
		id := uuid.NewString()
		must(t, w.st.Jobs.Create(ctx, store.Job{ID: id, Kind: kind, State: "done", Payload: payload, CreatedAt: time.Now()}))
		return id
	}
	push := add(string(kindPush), `{"org_id":"`+w.org+`","stack_id":"`+w.stack+`"}`)
	deploy := add(string(kindDeploy), `{"env_id":"`+w.env+`","org_id":"`+w.org+`"}`)
	none := add("backup", `{}`)

	js, err := w.orch.Jobs(ctx, 0)
	must(t, err)
	got := map[string]string{}
	for _, j := range js {
		got[j.ID] = j.Scope
	}
	if got[push] != st.Slug || got[deploy] != st.Slug+"/"+e.Slug || got[none] != "" {
		t.Errorf("scopes = %q, want push %q, deploy %q, none empty", got, st.Slug, st.Slug+"/"+e.Slug)
	}
}
