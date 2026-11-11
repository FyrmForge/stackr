package instance

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// The overview's endpoint and slices are live regions, a failed read shows
// one line, the Backups tab polls its window and the empty Logs tab polls
// until a container runs.
func TestStaticPartsAreLive(t *testing.T) {
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Base: "/o/s/e/-/instances/pg", Live: "/live"}
	o := OverviewView{Endpoint: "db:5432", Slices: []SliceRow{{Slice: "main", Name: "app"}}}
	page := render(Overview(v, o))
	for _, r := range []string{"overview", "slices"} {
		if !strings.Contains(page, `sse-swap="`+r+`"`) {
			t.Errorf("no %s region", r)
		}
	}
	if got := render(LiveOverview(v, o)); !strings.Contains(got, "db:5432") {
		t.Errorf("overview lacks the endpoint: %s", got)
	}
	bad := OverviewView{Unread: map[string]string{"overview": "x", "slices": "y"}}
	if got := render(LiveOverview(v, bad)); !strings.Contains(got, "read the instance: x") {
		t.Errorf("overview unread: %s", got)
	}
	if got := render(LiveSlices(v, bad)); !strings.Contains(got, "read the slices: y") {
		t.Errorf("slices unread: %s", got)
	}
	if got := render(Backups(v, PITRView{}, nil)); !strings.Contains(got, `hx-select="#pitr"`) {
		t.Errorf("backups do not poll the pitr panel: %s", got)
	}
	if got := render(Logs(v, LogsView{})); !strings.Contains(got, `hx-trigger="every 3s"`) {
		t.Errorf("empty logs do not poll: %s", got)
	}
	if got := render(Logs(v, LogsView{Running: true})); strings.Contains(got, "every 3s") {
		t.Errorf("running logs poll: %s", got)
	}
}
