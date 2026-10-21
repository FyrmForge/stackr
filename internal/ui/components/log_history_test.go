package components

import (
	"context"
	"strings"
	"testing"
)

// The env viewer has the tile picker and an Older button when a page is full;
// a tile viewer has no picker, keeps its tile in a hidden field and offers
// Live; stderr and access lines are marked.
func TestLogHistory(t *testing.T) {
	render := func(v LogHistoryView) string {
		var b strings.Builder
		if err := LogHistory(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	lines := []LogHistoryLine{
		{Time: "Oct 21 10:00:00", Tile: "api", Source: "access", Text: "GET /a 200 12ms"},
		{Time: "Oct 21 09:59:59", Tile: "api", Source: "app", Text: "boom", Err: true},
	}
	env := render(LogHistoryView{
		Action: "/a/s/e/-/logs", Range: "1h", Lines: lines, Older: "/a/s/e/-/logs?before=x",
		Tiles: []LogOption{{"", "all tiles"}, {"api", "api"}},
	})
	for _, w := range []string{
		`hx-get="/a/s/e/-/logs"`, `name="tile"`, `name="kind"`, `name="since"`, `<option value="1h" selected>`,
		`GET /a 200 12ms`, `text-rw-danger`, `id="log-older"`, `hx-get="/a/s/e/-/logs?before=x"`,
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
}
