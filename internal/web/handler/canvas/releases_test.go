package canvas_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The env drawer's releases tab lists the stack's releases, marks the one
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
	if !strings.Contains(body, ">current<") || !strings.Contains(body, ">#1<") {
		t.Errorf("releases tab:\n%s", body)
	}
	body = get(t, s, base+"?tab=releases&plan="+id)
	if !strings.Contains(body, "release #1") || !strings.Contains(body, base+"/promote/"+id) {
		t.Errorf("dry run:\n%s", body)
	}
	if body = get(t, s, base+"?tab=releases&plan=nope"); strings.Contains(body, "release #") {
		t.Errorf("an unknown release was planned:\n%s", body)
	}
	rec := s.Do(t, "POST", base+"/promote/"+id, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Queued") || !strings.Contains(rec.Body.String(), "/-/jobs/") ||
		!strings.Contains(rec.Body.String(), `hx-get="`+base+`?tab=releases" hx-trigger="sse:end"`) {
		t.Errorf("promote = %d\n%s", rec.Code, rec.Body)
	}
}
