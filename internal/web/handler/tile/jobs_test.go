package tile

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// Each Jobs row links to its job's log page under the env, not the tile.
func TestJobsViewLinksToTheLog(t *testing.T) {
	v := jobsView("/acme/shop/dev/-/tiles/api", "", []service.Job{{ID: "j1", Kind: "deploy"}})
	if got := v.Rows[0].Href; got != "/acme/shop/dev/-/jobs/j1" {
		t.Errorf("href = %q", got)
	}
}
