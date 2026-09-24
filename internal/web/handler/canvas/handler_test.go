package canvas_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/handler/canvas"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func get(t *testing.T, s *webtest.Site, path string) string {
	t.Helper()
	rec := s.Do(t, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// Every level renders its canvas with the cards the service decided.
func TestCanvasLevels(t *testing.T) {
	s := webtest.New(t)
	for path, want := range map[string]string{
		"/":              `node-id="org:` + s.Org + `"`,
		"/acme":          `node-id="stack:` + s.Tile.Stack + `"`,
		"/acme/shop":     `node-id="env:` + s.Tile.Env + `"`,
		"/acme/shop/dev": `node-id="` + s.Tile.ID + `"`,
	} {
		body := get(t, s, path)
		if !strings.Contains(body, "<graph-canvas") || !strings.Contains(body, want) {
			t.Errorf("%s: no canvas with %s in\n%s", path, want, body)
		}
	}
	if body := get(
		t,
		s,
		"/acme/shop/dev?system=0",
	); !strings.Contains(body, `hx-post="/acme/shop/dev/-/positions?system=0"`) {
		t.Error("the show params are not kept on the helper routes")
	}
}

// Home is the caller's own canvas: anonymous goes to the login page.
func TestCanvasHomeNeedsLogin(t *testing.T) {
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	rec := httptest.NewRecorder()
	srv.Echo().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound ||
		rec.Header().Get("Location") != "/login" {
		t.Errorf("anonymous / = %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// A drop saves; notes round-trip and answer the canvas.
func TestCanvasPositionsAndNotes(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "POST", "/acme/-/positions", url.Values{
		"node_id": {"stack:" + s.Tile.Stack},
		"x":       {"440"},
		"y":       {"88"},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("positions = %d %s", rec.Code, rec.Body)
	}
	if body := get(t, s, "/acme"); !strings.Contains(body, `x="440" y="88"`) {
		t.Error("the drop did not stick")
	}
	if rec := s.Do(t, "POST", "/acme/-/positions", url.Values{
		"node_id": {"stack:nope"},
		"x":       {"1"},
		"y":       {"1"},
	}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown card = %d", rec.Code)
	}
	rec = s.Do(t, "POST", "/acme/-/notes", url.Values{
		"kind": {"note"},
		"text": {"hello there"},
		"w":    {"160"},
		"h":    {"80"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello there</textarea>") {
		t.Fatalf("note = %d %s", rec.Code, rec.Body)
	}
	id := regexp.MustCompile(`node-id="note:([^"]+)"`).FindStringSubmatch(rec.Body.String())[1]
	rec = s.Do(t, "POST", "/acme/-/notes/delete", url.Values{"id": {id}})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "note:"+id) {
		t.Errorf("delete note = %d, still drawn: %v", rec.Code, strings.Contains(rec.Body.String(), id))
	}
	if rec := s.Do(t, "POST", "/acme/-/reset", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "<graph-canvas") {
		t.Errorf("reset = %d", rec.Code)
	}
}

// The stream sends every footer at connect, then only the ones that move:
// a failed job turns the env card's footer to "error".
func TestCanvasEventsSwapFooter(t *testing.T) {
	stream.PollEvery, canvas.Every = 10*time.Millisecond, 0
	s := webtest.New(t)
	events := html.UnescapeString(
		regexp.MustCompile(`sse-connect="([^"]+)"`).FindStringSubmatch(get(t, s, "/acme/shop"))[1],
	)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(60*time.Millisecond, func() { s.FailedJob(t, s.Tile.ID) })
	time.AfterFunc(250*time.Millisecond, cancel)
	body := s.DoCtx(ctx, t, "GET", events, nil).Body.String()
	name := "event: footer:env:" + s.Tile.Env + "\n"
	if strings.Count(body, name) != 2 || strings.Contains(body, "event: graph") {
		t.Fatalf("want the env footer at connect and once more on change, no graph event:\n%s", body)
	}
	last := body[strings.LastIndex(body, name):]
	if !strings.Contains(last, `sse-swap="footer:env:`+s.Tile.Env+`"`) || !strings.Contains(last, ">error<") {
		t.Errorf("second footer is not the error one:\n%s", last)
	}
}

// A card added under the page swaps the whole canvas.
func TestCanvasEventsSwapGraph(t *testing.T) {
	stream.PollEvery, canvas.Every = 10*time.Millisecond, 0
	s := webtest.New(t)
	events := html.UnescapeString(regexp.MustCompile(`sse-connect="([^"]+)"`).FindStringSubmatch(get(t, s, "/acme"))[1])
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(60*time.Millisecond, func() { _, _ = s.Orch.CreateStack(context.Background(), s.Org, "blog", "") })
	time.AfterFunc(250*time.Millisecond, cancel)
	body := s.DoCtx(ctx, t, "GET", events, nil).Body.String()
	if !strings.Contains(body, "event: graph\ndata: <graph-canvas") || !strings.Contains(body, ">blog</a>") {
		t.Fatalf("no graph event with the new stack:\n%s", body)
	}
}

// The env canvas draws a lanes layer and its one stream carries the
// footers and the rendered lanes ("traffic", HTML, not JSON).
func TestEnvEventsCarryLanes(t *testing.T) {
	stream.PollEvery, canvas.Every = 10*time.Millisecond, 0
	s := webtest.New(t)
	page := get(t, s, "/acme/shop/dev")
	if !strings.Contains(page, `id="graph-lanes"`) ||
		!strings.Contains(page, `sse-connect="/acme/shop/dev/-/events?n=`) {
		t.Fatalf("env page has no lanes layer or stream under /-/:\n%s", page)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	body := s.DoCtx(ctx, t, "GET", "/acme/shop/dev/-/events", nil).Body.String()
	if !strings.Contains(body, "event: traffic\ndata: <svg data-edges data-lanes id=\"graph-lanes\"") ||
		!strings.Contains(body, "event: footer:"+s.Tile.ID) {
		t.Fatalf("env events:\n%s", body)
	}
	if strings.Contains(get(t, s, "/acme/shop/dev?traffic=0"), `id="graph-lanes"`) {
		t.Error("traffic=0 still draws lanes")
	}
}

// Env nodes wear session C's cards: the body opens the tile drawer under
// /-/, and the footer is the card's own (the stream re-sends it).
func TestEnvCardsAreTileCards(t *testing.T) {
	s := webtest.New(t)
	body := get(t, s, "/acme/shop/dev")
	for _, want := range []string{
		`hx-get="/acme/shop/dev/-/tiles/api?tab=status"`,
		`hx-push-url="?drawer=` + s.Tile.ID + `&amp;tab=status"`,
		`sse-swap="footer:` + s.Tile.ID + `"`,
		`hx-get="/acme/shop/dev/-/new-tile"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("env page lacks %s", want)
		}
	}
}

// A fresh load of ?drawer=<tile id>&tab= on the env page comes with the
// drawer open, its route loading the asked tab.
func TestEnvFreshLoadOpensTileDrawer(t *testing.T) {
	s := webtest.New(t)
	load := func(path string) string { return s.DoNoCSRF(t, "GET", path).Body.String() } // no HX-Request: a full load
	body := load("/acme/shop/dev?drawer=" + s.Tile.ID + "&tab=logs")
	if !regexp.MustCompile(`<side-drawer open tab="logs"`).MatchString(body) ||
		!strings.Contains(body, `hx-get="/acme/shop/dev/-/tiles/api?tab=logs" hx-trigger="load"`) {
		t.Errorf("drawer not open on the tile's logs:\n%s", body)
	}
	if body := load("/acme/shop/dev?drawer=nope"); strings.Contains(body, "<side-drawer open") {
		t.Error("an unknown id opened the drawer")
	}
}
