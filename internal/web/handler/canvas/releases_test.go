package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/web/handler/canvas"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The env drawer's releases tab lists the stack's releases, chips the one
// the env runs, dry-runs taking one and queues the promote it confirms.
func TestEnvReleases(t *testing.T) {
	s := webtest.New(t)
	s.Healthy(s.Tile.Env)
	ctx := context.Background()
	j, err := s.Orch.Deploy(ctx, s.Tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if j, err = s.Orch.GetJob(ctx, j.ID); err != nil || j.FinishedAt != nil || time.Now().After(end) {
			break
		}
	}
	if j.State != "done" {
		t.Fatalf("deploy = %s", j.State)
	}
	rs, err := s.Orch.Releases(ctx, s.Tile.Stack)
	if err != nil || len(rs) != 1 {
		t.Fatalf("releases = %v, %v", rs, err)
	}
	id := rs[0].ID
	base := "/acme/shop/dev/-/drawer"
	body := get(t, s, base+"?tab=releases")
	if !strings.Contains(body, `env-dot env-c-violet`) || !strings.Contains(body, ">release 1<") {
		t.Errorf("releases tab:\n%s", body)
	}
	if strings.Contains(body, `<ul class="flex flex-wrap gap-2 mb-6">`) {
		t.Errorf("env releases tab repeats the env chips:\n%s", body)
	}
	if d := get(t, s, base+"?tab=releases"); !strings.Contains(d, `id="drawer-title" class="text-lg font-semibold text-rw-text truncate sr-only"`) {
		t.Errorf("env drawer title is visible:\n%s", d)
	}
	if st := get(t, s, "/acme/shop/-/drawer?tab=releases"); !strings.Contains(st, `<ul class="flex flex-wrap gap-2 mb-6">`) {
		t.Errorf("stack releases tab lost its ladder:\n%s", st)
	}
	body = get(t, s, base+"?tab=releases&plan="+id)
	if !strings.Contains(body, "Release 1 for dev") || !strings.Contains(body, `hx-post="`+base+"/promote/"+id+`"`) || !strings.Contains(body, ">Promote to dev<") || strings.Contains(body, "<confirm-dialog") {
		t.Errorf("dry run:\n%s", body)
	}
	if strings.Contains(body, "hx-trigger=\"every 3s\"") && strings.Contains(body, "plan="+id) {
		t.Errorf("an idle review polls:\n%s", body)
	}
	if strings.Contains(body, "hx-include") { // no removal rows: no ticks to include
		t.Errorf("a plan with no removals includes ticks:\n%s", body)
	}
	// the release modal is its own route; the review page stays at the id
	body = get(t, s, "/acme/shop/dev/-/releases/"+id+"/dialog")
	if !strings.Contains(body, `id="rel-modal"`) || !strings.Contains(body, ">shop<") || !strings.Contains(body, "Release 1") || !strings.Contains(body, `href="/acme/shop/dev/-/releases/`+id+`"`) {
		t.Errorf("release modal:\n%s", body)
	}
	if body = get(t, s, base+"?tab=releases&plan=nope"); strings.Contains(body, "Release 1 for") {
		t.Errorf("an unknown release was planned:\n%s", body)
	}
	rec := s.Do(t, "POST", base+"/promote/"+id, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Queued") ||
		!strings.Contains(rec.Body.String(), "/-/jobs/") ||
		!strings.Contains(rec.Body.String(), `hx-get="`+base+`?tab=releases" hx-trigger="sse:end"`) {
		t.Errorf("promote = %d\n%s", rec.Code, rec.Body)
	}
	// the ticked removal rows ride the form into the job
	s.Do(t, "POST", base+"/promote/"+id, url.Values{"ticked": {"param:app.pw", "param:pr.app.pw"}})
	js, _ := s.Orch.Jobs(ctx, 20)
	var got bool
	for _, j := range js {
		got = got || strings.Contains(j.Payload, `"ticked":["param:app.pw","param:pr.app.pw"]`)
	}
	if !got {
		t.Errorf("no promote job carries the ticks: %+v", js)
	}
}

// The stack drawer's Promote names the env by slug, and the route takes it.
func TestStackReleasesPromote(t *testing.T) {
	s := webtest.New(t)
	s.Healthy(s.Tile.Env)
	ctx := context.Background()
	j, err := s.Orch.Deploy(ctx, s.Tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if j, err = s.Orch.GetJob(ctx, j.ID); err != nil || j.FinishedAt != nil || time.Now().After(end) {
			break
		}
	}
	rs, err := s.Orch.Releases(ctx, s.Tile.Stack)
	if err != nil || len(rs) != 1 {
		t.Fatalf("releases = %v, %v", rs, err)
	}
	id := rs[0].ID
	base := "/acme/shop/-/drawer"
	body := get(t, s, base+"?tab=releases&env="+s.Tile.Env+"&plan="+id)
	if !strings.Contains(body, base+"/promote/dev/"+id) {
		t.Errorf("dry run lacks the slug url:\n%s", body)
	}
	rec := s.Do(t, "POST", base+"/promote/dev/"+id, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Queued") {
		t.Errorf("promote = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.Do(t, "POST", base+"/promote/"+s.Tile.Env+"/"+id, nil); rec.Code != 404 {
		t.Errorf("promote by env id = %d, want 404", rec.Code)
	}
	// a viewer, who cannot finish a move, is offered none
	stagingOf(t, s)
	if !strings.Contains(get(t, s, base+"?tab=releases"), "tab=releases&amp;env=") {
		t.Error("an owner is offered no move")
	}
	v := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, v, "viewer")
	body = s.As(t, s.Session(t, v), "GET", base+"?tab=releases", nil).Body.String()
	if !strings.Contains(body, "#1") || strings.Contains(body, "tab=releases&amp;env=") {
		t.Errorf("a viewer sees Promote or Roll back:\n%s", body)
	}
}

// A failed push of the stack shows over the env page and its Releases tab,
// and on the stack's card, until a later push does not fail.
func TestFailedPushShows(t *testing.T) {
	s := webtest.New(t)
	at := time.Now()
	id := s.PushJob(t, s.Tile.Stack, "failed", "build x: buildx build: exit status 1", at)
	for _, path := range []string{"/acme/shop/dev", "/acme/shop/dev/-/drawer?tab=releases"} {
		body := get(t, s, path)
		if !strings.Contains(body, "The last push failed: build x: buildx build: exit status 1.") ||
			!strings.Contains(body, `href="/acme/shop/dev/-/jobs/`+id+`"`) {
			t.Errorf("%s lacks the failed push:\n%s", path, body)
		}
	}
	if body := get(t, s, "/acme"); !strings.Contains(body, ">Push failed<") || !strings.Contains(body, "exit status 1</span>") || !strings.Contains(body, `href="/acme/-/jobs/`+id+`"`) || strings.Contains(body, `<a href="/acme/shop"`) {
		t.Errorf("stack card:\n%s", body)
	}
	s.PushJob(t, s.Tile.Stack, "done", "", at.Add(time.Minute))
	if body := get(t, s, "/acme/shop/dev/-/drawer?tab=releases"); strings.Contains(body, "The last push failed") {
		t.Errorf("a later push left the failure:\n%s", body)
	}
}

// The releases window sits on the stack and env canvas in the pill's place:
// a failed push is a counted row with its job log, ?rel=1 opens it, and the
// canvas stream carries its rows as the "releases" event.
func TestReleasesWindow(t *testing.T) {
	stream.PollEvery, canvas.Every = 10*time.Millisecond, 0
	s := webtest.New(t)
	id := s.PushJob(t, s.Tile.Stack, "failed", "build x: exit status 1", time.Now())
	for _, path := range []string{"/acme/shop/dev", "/acme/shop"} {
		body := get(t, s, path)
		for _, want := range []string{`id="rel-window"`, ">1 failed<", "Push failed: build x: exit status 1", `/-/jobs/` + id + `"`, `hx-get="/acme/shop/dev"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
		if strings.Contains(body, `class="env-compare`) && strings.Contains(body, `<details id="rel-window"`) && strings.Contains(body, `z-10 open:w-[34rem] open:max-w-[calc(100%-2rem)]" open>`) {
			t.Errorf("%s: the window opened without ?rel=1", path)
		}
	}
	if !strings.Contains(get(t, s, "/acme/shop/dev?rel=1"), `open:max-w-[calc(100%-2rem)]" open>`) {
		t.Error("?rel=1 did not open the window")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	if body := s.DoCtx(ctx, t, "GET", "/acme/shop/dev/-/events", nil).Body.String(); !strings.Contains(body, "event: releases\ndata: <ul") || !strings.Contains(body, `id="rel-counts" hx-swap-oob="outerHTML"`) {
		t.Errorf("no releases event:\n%s", body)
	}
}

// A running push shows where its release will show: the env page, its
// Releases tab and the stack's card; a finished one takes the line away. A
// stack with no environments says it on its own page, and says why a done push
// made none.
func TestRunningPushShows(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	id := s.LivePushJob(t, s.Tile.Stack, "running", `{"stack_id":"`+s.Tile.Stack+`","event":{"Commit":"f9cdc50abc"}}`)
	for _, path := range []string{"/acme/shop/dev", "/acme/shop/dev/-/drawer?tab=releases"} {
		body := get(t, s, path)
		if !strings.Contains(body, "Building shop at f9cdc50, started ") || !strings.Contains(body, `href="/acme/shop/dev/-/jobs/`+id+`"`) {
			t.Errorf("%s lacks the build line:\n%s", path, body)
		}
	}
	if body := get(t, s, "/acme"); !strings.Contains(body, "Building at f9cdc50, started ") || !strings.Contains(body, `href="/acme/-/jobs/`+id+`"`) {
		t.Errorf("stack card:\n%s", body)
	}
	s.EndJob(t, id, "done", "", "")
	if body := get(t, s, "/acme/shop/dev/-/drawer?tab=releases"); strings.Contains(body, "Building shop") {
		t.Errorf("a finished push left the line:\n%s", body)
	}

	empty, err := s.Orch.CreateStack(ctx, s.Org, "huginn", "")
	if err != nil {
		t.Fatal(err)
	}
	pid := s.LivePushJob(t, empty.ID, "running", `{"event":{"Commit":"abc1234def"}}`)
	if body := get(t, s, "/acme/huginn"); !strings.Contains(body, "Building huginn at abc1234, started ") || !strings.Contains(body, `href="/acme/-/jobs/`+pid+`"`) {
		t.Errorf("empty stack, running:\n%s", body)
	}
	s.EndJob(t, pid, "done", "", `{"skipped":["prod: the file gives it no branch or promote"]}`)
	if body := get(t, s, "/acme/huginn"); !strings.Contains(body, "This stack has no environments yet.") || !strings.Contains(body, "prod: the file gives it no branch or promote") {
		t.Errorf("empty stack, done:\n%s", body)
	}
	s.EndJob(t, pid, "failed", "buildx build: exit status 1", "")
	if body := get(t, s, "/acme/huginn"); !strings.Contains(body, "The last push failed: buildx build: exit status 1.") {
		t.Errorf("empty stack, failed:\n%s", body)
	}
}

// A job parked on an unset value says so with a link to set it: over the env
// page and its Releases tab, on the stack's card, and on the tile that never
// deployed.
func TestParkedValueShows(t *testing.T) {
	s := webtest.New(t)
	s.WaitingJob(t, "huginn.oidc_secret", `{"env_id":"`+s.Tile.Env+`"}`)
	for _, path := range []string{"/acme/shop/dev", "/acme/shop/dev/-/drawer?tab=releases"} {
		body := get(t, s, path)
		if !strings.Contains(body, "Waiting for huginn.oidc_secret") || !strings.Contains(body, "&amp;tab=variables") || !strings.Contains(body, ">Set it<") {
			t.Errorf("%s lacks the parked line:\n%s", path, body)
		}
	}
	if body := get(t, s, "/acme"); !strings.Contains(body, "Waiting for huginn.oidc_secret") || !strings.Contains(body, ">Set it<") {
		t.Errorf("stack card:\n%s", body)
	}
}

// A release cut before the author column (empty CreatedBy) is a push: its
// commit and message still show (the test seeds the message) in the drawer's releases tab.
func TestEnvReleasesLegacyPushShowsCommit(t *testing.T) {
	s := webtest.New(t)
	s.Healthy(s.Tile.Env)
	ctx := context.Background()
	j, err := s.Orch.Deploy(ctx, s.Tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if j, err = s.Orch.GetJob(ctx, j.ID); err != nil || j.FinishedAt != nil || time.Now().After(end) {
			break
		}
	}
	rs, err := s.Orch.Releases(ctx, s.Tile.Stack)
	if err != nil || len(rs) != 1 {
		t.Fatalf("releases = %v, %v (deploy %s)", rs, err, j.State)
	}
	id := rs[0].ID
	r, err := s.Store.Releases.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.CreatedBy, r.Message = "", "legacy subject line" // the commit is derived, not stored
	if err := s.Store.Releases.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/acme/shop/dev/-/drawer?tab=releases")
	if !strings.Contains(body, "legacy subject line") {
		t.Errorf("legacy push lost its message:\n%s", body)
	}
}
