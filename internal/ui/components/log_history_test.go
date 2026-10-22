package components

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The env viewer has the tile picker and an Older button when a page is full;
// a tile viewer has no picker, keeps its tile in a hidden field and offers
// Live; stderr and access lines are marked. A line's time is a <time> in UTC
// that <local-time> rewrites in the viewer's zone.
func TestLogHistory(t *testing.T) {
	render := func(v LogHistoryView) string {
		var b strings.Builder
		if err := LogHistory(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	lines := []LogHistoryLine{
		{Time: time.Date(2026, 10, 21, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600)), Tile: "api", Source: "access", Text: "GET /a 200 12ms"},
		{Time: time.Date(2026, 10, 21, 9, 59, 59, 0, time.UTC), Tile: "api", Source: "app", Text: "boom", Err: true},
	}
	env := render(LogHistoryView{
		Action: "/a/s/e/-/logs", Range: "1h", Lines: lines, Older: "/a/s/e/-/logs?before=x",
		Tiles: []LogOption{{"", "all tiles"}, {"api", "api"}},
	})
	for _, w := range []string{
		`hx-get="/a/s/e/-/logs"`, `name="tile"`, `name="kind"`, `name="since"`, `<option value="1h" selected>`,
		`GET /a 200 12ms`, `text-rw-danger`, `Times in <local-time zone>UTC</local-time>`,
		`<local-time fmt="Jan 02 15:04:05"><time datetime="2026-10-21T10:00:00Z" title="2026-10-21T10:00:00Z">Oct 21 10:00:00</time></local-time>`, `id="log-older"`, `hx-get="/a/s/e/-/logs?before=x"`,
	} {
		if !strings.Contains(env, w) {
			t.Errorf("env viewer lacks %s\n%s", w, env)
		}
	}
	tile := render(LogHistoryView{Action: "/a/s/e/-/logs", Tile: "api", Live: "/a/s/e/-/tiles/api?tab=logs"})
	for _, w := range []string{`name="from" value="tile"`, `name="tile" value="api"`, `>Live</button>`, `No lines in this range.`} {
		if !strings.Contains(tile, w) {
			t.Errorf("tile viewer lacks %s", w)
		}
	}
	if strings.Contains(tile, `name="kind"`) || strings.Contains(tile, `log-older`) {
		t.Error("tile viewer has the env's filters or an Older button")
	}
	if f := render(LogHistoryView{Action: "/a/s/e/-/logs", Source: "access", Filtered: true}); !strings.Contains(f, "No lines match these filters.") {
		t.Errorf("a filtered empty page reads\n%s", f)
	}
	// the live tail names its zone as History does
	var b strings.Builder
	if err := LogPane(LogPaneView{StreamURL: "/s"}).Render(context.Background(), &b); err != nil || !strings.Contains(b.String(), `Times in <local-time zone>UTC</local-time>`) {
		t.Errorf("live tail = %v\n%s", err, b.String())
	}
}
