package tile

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Each Jobs row links to its job's log page under the env, not the tile.
func TestJobsViewLinksToTheLog(t *testing.T) {
	v := jobsView("/acme/shop/dev/-/tiles/api", "api", "", false, []service.Job{{ID: "j1", Kind: "deploy"}})
	if got := v.Rows[0].Href; got != "/acme/shop/dev/-/jobs/j1" {
		t.Errorf("href = %q", got)
	}
}

// A failed promote row offers Retry, the tile's normal deploy.
func TestJobsViewRetryOnFailedPromote(t *testing.T) {
	js := []service.Job{{ID: "j1", Kind: "promote", State: "failed"}, {ID: "j2", Kind: "promote", State: "done"}}
	v := jobsView("/acme/shop/dev/-/tiles/api", "api", "", true, js)
	if got := v.Rows[0].Retry; got != "/acme/shop/dev/-/tiles/api/deploy" {
		t.Errorf("retry = %q", got)
	}
	if v.Rows[1].Retry != "" {
		t.Errorf("done row has retry %q", v.Rows[1].Retry)
	}
}

// Retry sits on the newest row only, and "failed here" only on the row that
// stopped at this tile.
func TestJobsViewRetryOnlyOnTheNewestFailureAndHere(t *testing.T) {
	stopped := func(tile string) string { return `{"stopped_at":"` + tile + `"}` }
	js := []service.Job{
		{ID: "j1", Kind: "deploy", State: "done"},
		{ID: "j2", Kind: "promote", State: "failed", Payload: stopped("api")},
		{ID: "j3", Kind: "promote", State: "failed", Payload: stopped("web")},
	}
	v := jobsView("/acme/shop/dev/-/tiles/api", "api", "", true, js)
	for i, r := range v.Rows {
		if r.Retry != "" {
			t.Errorf("row %d retry = %q, want none after a newer job", i, r.Retry)
		}
	}
	if v.Rows[0].Here || !v.Rows[1].Here || v.Rows[2].Here {
		t.Errorf("here = %v %v %v", v.Rows[0].Here, v.Rows[1].Here, v.Rows[2].Here)
	}
}
