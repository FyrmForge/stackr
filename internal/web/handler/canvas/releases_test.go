package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

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
	if !strings.Contains(body, `log-chip env-c-violet">dev<`) || !strings.Contains(body, ">#1<") {
		t.Errorf("releases tab:\n%s", body)
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
