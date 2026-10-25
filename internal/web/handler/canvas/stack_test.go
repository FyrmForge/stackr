package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The stack's Settings tab is one accordion holding what were the Params
// and Placement tabs; Env order moves the ladder from here.
func TestStackSettingsRows(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	const d = "/acme/shop/-/drawer"
	body := get(t, s, d+"?tab=settings&open=order")
	for _, w := range []string{
		`data-acc="order" open`, "dev, staging",
		`data-acc="variables"`, `id="vars-editor"`,
		`data-acc="placement"`, `hx-get="/acme/shop/-/placement"`, "inherits org (default)",
		`data-acc="config"`, "managed from the UI",
		`data-acc="resources"`, "inherits org",
		`data-acc="host"`, `data-acc="delete"`,
		`hx-post="` + d + `/order"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s in\n%s", w, body)
		}
	}
	for _, gone := range []string{"tab=params", "tab=placement"} {
		if strings.Contains(body, gone) {
			t.Errorf("the drawer still offers %s", gone)
		}
	}
	rec := s.Do(t, "POST", d+"/order", url.Values{"ids": {st.ID, s.Tile.Env}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "staging, dev") {
		t.Errorf("reorder = %d\n%s", rec.Code, rec.Body)
	}
	es, err := s.Orch.Ladder(context.Background(), s.Tile.Stack)
	if err != nil || len(es) != 2 || es[0].ID != st.ID {
		t.Errorf("ladder after the move = %v, %v", es, err)
	}
}
