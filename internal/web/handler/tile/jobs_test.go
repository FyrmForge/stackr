package tile

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Each Jobs row links to its job's log page under the env, not the tile.
func TestJobsViewLinksToTheLog(t *testing.T) {
	v := jobsView("/acme/shop/dev/-/tiles/api", "", false, []service.Job{{ID: "j1", Kind: "deploy"}})
	if got := v.Rows[0].Href; got != "/acme/shop/dev/-/jobs/j1" {
		t.Errorf("href = %q", got)
	}
}

// A failed promote row offers Retry, the tile's normal deploy.
func TestJobsViewRetryOnFailedPromote(t *testing.T) {
	js := []service.Job{{ID: "j1", Kind: "promote", State: "failed"}, {ID: "j2", Kind: "promote", State: "done"}}
	v := jobsView("/acme/shop/dev/-/tiles/api", "", true, js)
	if got := v.Rows[0].Retry; got != "/acme/shop/dev/-/tiles/api/deploy" {
		t.Errorf("retry = %q", got)
	}
	if v.Rows[1].Retry != "" {
		t.Errorf("done row has retry %q", v.Rows[1].Retry)
	}
}
