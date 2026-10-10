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
	if !strings.Contains(body, "Promote #1 to dev?") || !strings.Contains(body, base+"/promote/"+id) {
		t.Errorf("dry run:\n%s", body)
	}
	if !strings.Contains(body, `hx-include="find [name=ticked]"`) {
		t.Errorf("the promote form does not carry the plan's ticks:\n%s", body)
	}
	if body = get(t, s, base+"?tab=releases&plan=nope"); strings.Contains(body, "Promote #") {
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
}
