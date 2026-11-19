package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A queued or running push is the stack's build in flight, and only that
// stack's; the sha is short, a running one beats a queued one behind it, and
// a finished, failed or superseded one is not building.
func TestBuilding(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	push := func(stack, state, sha string) string {
		at = at.Add(time.Minute)
		id := uuid.NewString()
		if err := env.Store.Jobs.Create(ctx, store.Job{ID: id, Kind: "push", State: state,
			LockSet: store.StringList{"push:" + stack, "push:" + stack + ":acme/app@main"},
			Payload: `{"stack_id":"` + stack + `","event":{"Commit":"` + sha + `"}}`, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if _, ok := env.Orch.Building(ctx, "s1"); ok {
		t.Fatal("a stack with no push is not building")
	}
	push("s1", "done", "aaaaaaaaaaaa")
	push("s1", "superseded", "bbbbbbbbbbbb")
	if _, ok := env.Orch.Building(ctx, "s1"); ok {
		t.Error("a finished push is not building")
	}
	q := push("s1", "queued", "f9cdc50123456")
	b, ok := env.Orch.Building(ctx, "s1")
	if !ok || b.JobID != q || b.Sha != "f9cdc50" || !b.Queued {
		t.Errorf("queued = %+v, %v", b, ok)
	}
	r := push("s1", "running", "cccccccccccc")
	push("s1", "queued", "dddddddddddd")
	if b, ok = env.Orch.Building(ctx, "s1"); !ok || b.JobID != r || b.Queued {
		t.Errorf("running = %+v, %v", b, ok)
	}
	if _, ok := env.Orch.Building(ctx, "s2"); ok {
		t.Error("another stack's build leaks")
	}
	w := push("s3", "waiting", "wwwwwwwwwwww")
	if _, ok := env.Orch.Building(ctx, "s3"); ok {
		t.Error("a waiting push shows as building")
	}
	if p, ok := env.Orch.PushState(ctx, "s3"); !ok || p.JobID != w || p.Queued || p.Waits == "" {
		t.Errorf("waiting = %+v, %v", p, ok)
	}
	push("s1", "failed", "eeeeeeeeeeee")
	if _, ok := env.Orch.Building(ctx, "s1"); !ok {
		t.Error("an older running push stopped being the build")
	}
}

// A done push says why it made no env, from its payload; a failed one says
// its error.
func TestPushStateSkipped(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	if err := env.Store.Jobs.Create(ctx, store.Job{ID: uuid.NewString(), Kind: "push", State: "done", LockSet: store.StringList{"push:s1"},
		Payload: `{"stack_id":"s1","skipped":["prod: the file gives it no branch or promote"]}`, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, ok := env.Orch.PushState(ctx, "s1")
	if !ok || !p.Done || len(p.Skipped) != 1 || !strings.HasPrefix(p.Skipped[0], "prod:") {
		t.Errorf("state = %+v, %v", p, ok)
	}
}

// A parked promote or deploy reports its value for its env and its stack, and
// not for another stack; the link sets it (an org value in the org's
// variables). The job list names it in the WAITS column.
func TestParkedOn(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	tl := env.Tile(t, org)
	env.WaitingJob(t, "huginn.oidc_secret", `{"env_id":"`+tl.Env+`"}`)
	env.WaitingJob(t, "org.gate.password", `{"tile_id":"`+tl.ID+`"}`)
	for _, envID := range []string{"", tl.Env} {
		ps := env.Orch.ParkedOn(ctx, tl.Stack, envID)
		if len(ps) != 2 || ps[0].Param != "huginn.oidc_secret" || !strings.HasSuffix(ps[0].Href, "&tab=variables") || !strings.Contains(ps[0].Href, "drawer=env%3A") ||
			ps[1].Param != "org.gate.password" || !strings.Contains(ps[1].Href, "drawer=org%3A") {
			t.Errorf("env %q: %+v", envID, ps)
		}
	}
	if ps := env.Orch.ParkedOn(ctx, "other", ""); len(ps) != 0 {
		t.Errorf("another stack's wait leaks: %+v", ps)
	}
	if ps := env.Orch.ParkedOn(ctx, tl.Stack, "another-env"); len(ps) != 0 {
		t.Errorf("another env's wait leaks: %+v", ps)
	}
	js, err := env.Orch.Jobs(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, j := range js {
		if j.WaitingParam != nil {
			seen++
			if j.Waits != "Waits for "+*j.WaitingParam {
				t.Errorf("waits = %q for %q", j.Waits, *j.WaitingParam)
			}
		}
	}
	if seen != 2 {
		t.Errorf("listed %d parked jobs", seen)
	}
}

// The stack card says the build in flight, else the value a parked job
// waits for, and a tile of an env whose first promote is parked says it too.
func TestCanvasStackNote(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	scope := service.CanvasScope{Kind: service.CanvasOrg, ID: w.acme}
	card := func() service.GraphNode {
		v, err := w.e.Orch.Canvas(ctx, scope, service.ShowAll)
		if err != nil {
			t.Fatal(err)
		}
		return nodes(v)["stack:"+w.shop]
	}
	if n := card(); n.Note != "" {
		t.Fatalf("idle card says %q", n.Note)
	}
	w.e.WaitingJob(t, "huginn.oidc_secret", `{"env_id":"`+w.prod+`"}`)
	if n := card(); n.Note != "Waiting for huginn.oidc_secret" || n.NoteSet == "" || n.NoteJob != "" {
		t.Errorf("parked card = %+v", n)
	}
	id := uuid.NewString()
	if err := w.e.Store.Jobs.Create(ctx, store.Job{ID: id, Kind: "push", State: "running", Payload: `{"event":{"Commit":"f9cdc50999"}}`,
		LockSet: store.StringList{"push:" + w.shop}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if n := card(); n.Note != "Building at f9cdc50, started " || n.NoteJob != id || n.NoteAt.IsZero() {
		t.Errorf("building card = %+v", n)
	}
}

// A tile that never deployed reads "waiting" with the value, while its env's
// first promote is parked on it (the promote locked no tile of that env).
func TestCanvasTileWaitsOnParkedPromote(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	scope := service.CanvasScope{Kind: service.CanvasEnv, ID: w.dev}
	tile := func() service.GraphNode {
		v, err := w.e.Orch.Canvas(ctx, scope, service.ShowAll)
		if err != nil {
			t.Fatal(err)
		}
		return nodes(v)[w.worker]
	}
	if n := tile(); n.Status == "waiting" {
		t.Fatalf("worker waits with nothing parked: %+v", n)
	}
	w.e.WaitingJob(t, "huginn.oidc_secret", `{"env_id":"`+w.dev+`"}`)
	if n := tile(); n.Status != "waiting" || n.Waiting != "huginn.oidc_secret" {
		t.Errorf("worker = %q waiting for %q", n.Status, n.Waiting)
	}
}
