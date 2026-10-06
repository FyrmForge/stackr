package canvas_test

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// stagingOf adds staging beside dev, empty, so dev's api is New there.
func stagingOf(t *testing.T, s *webtest.Site) service.Environment {
	t.Helper()
	e, err := s.Orch.CreateEnv(
		context.Background(),
		s.Tile.Stack,
		"staging",
		service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The env page has a Sync button where the stack can sync, and not on a
// stack of one environment.
func TestEnvSyncButton(t *testing.T) {
	s := webtest.New(t)
	if body := get(t, s, "/acme/shop/dev"); strings.Contains(body, "?tab=sync") {
		t.Errorf("a one-env stack offers Sync:\n%s", body)
	}
	stagingOf(t, s)
	if body := get(
		t,
		s,
		"/acme/shop/staging",
	); !strings.Contains(body, `hx-get="/acme/shop/staging/-/drawer?tab=sync"`) {
		t.Errorf("no Sync button:\n%s", body)
	}
}

// ?sync= draws the review over the canvas: the New chip, a ghost card for
// the tile, the count in the bar; dropping the tile empties the count and
// takes Deploy away.
func TestEnvSyncCanvas(t *testing.T) {
	s := webtest.New(t)
	stagingOf(t, s)
	body := get(t, s, "/acme/shop/staging?sync=dev")
	for _, want := range []string{
		`node-id="sync:api"`,
		">New<",
		"1 tile from dev",
		`hx-post="/acme/shop/staging/-/drawer/sync/dev"`,
		"border-dashed opacity-70",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "/staging/-/tiles/api") {
		t.Error("the ghost opens a drawer for a tile that does not exist")
	}
	if strings.Contains(body, "sse-connect") {
		t.Error("the canvas streams while a review is open")
	}
	body = get(t, s, "/acme/shop/staging?sync=dev&drop=api")
	if !strings.Contains(body, "0 tiles from dev") || strings.Contains(body, "/drawer/sync/dev") ||
		strings.Contains(body, `node-id="sync:api"`) {
		t.Errorf("dropped:\n%s", body)
	}
	if rec := s.Do(t, "GET", "/acme/shop/staging?sync=nope", nil); rec.Code != 404 {
		t.Errorf("unknown source = %d", rec.Code)
	}
}

// The drawer's sync tab lists the tile with a Drop that, once taken, turns
// into an Undo; the page that moves the canvas brings the open drawer
// along.
func TestEnvSyncTab(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	base := "/acme/shop/staging/-/drawer?tab=sync"
	page := "/acme/shop/staging?drawer=env:" + st.ID + "&tab=sync&sync=dev"
	body := html.UnescapeString(get(t, s, base+"&sync=dev"))
	if !strings.Contains(body, `id="tab-sync"`) || !strings.Contains(body, ">api<") ||
		!strings.Contains(body, `href="`+page+`&drop=api"`) {
		t.Errorf("tab:\n%s", body)
	}
	if !strings.Contains(body, `hx-vals="{"keep":["api"],"sig":"`) {
		t.Errorf("deploy does not post the kept slugs and the sig:\n%s", body)
	}
	// no ?sync=: the default source, dev
	if def := get(t, s, base); !strings.Contains(def, `<option value="dev" selected>`) {
		t.Errorf("default source:\n%s", def)
	}
	body = html.UnescapeString(get(t, s, base+"&sync=dev&drop=api"))
	if !strings.Contains(body, "dropped") || !strings.Contains(body, `href="`+page+`"`) ||
		strings.Contains(body, "/drawer/sync/dev") {
		t.Errorf("dropped:\n%s", body)
	}
	// the env page, asked by htmx with the drawer named, sends it too
	body = get(t, s, page+"&drop=api")
	if !strings.Contains(body, `id="drawer-body" hx-swap-oob="innerHTML"`) || !strings.Contains(body, "dropped") {
		t.Errorf("the drawer did not follow the canvas:\n%s", body)
	}
	if body = get(t, s, "/acme/shop/staging?sync=dev"); strings.Contains(body, `id="drawer-body"`) {
		t.Error("a review with no drawer asked for sent one")
	}
}

// The bar's Deploy asks what the drawer's asks, and carries the kept slugs
// and the sig; a viewer's bar has none.
func TestEnvSyncBarConfirms(t *testing.T) {
	s := webtest.New(t)
	stagingOf(t, s)
	body := html.UnescapeString(get(t, s, "/acme/shop/staging?sync=dev"))
	_, bar, ok := strings.Cut(body, `id="sync-bar"`)
	if !ok {
		t.Fatalf("no bar:\n%s", body)
	}
	for _, want := range []string{
		"<confirm-dialog",
		"Sync 1 tile from dev into",
		"Rollback cannot undo a sync.",
		`"keep":["api"]`,
		`"sig":"`,
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("no %q in the bar:\n%s", want, bar)
		}
	}
	// the drawer may be closed: the bar's answer swaps nothing, posts bar=1
	if strings.Contains(bar, `hx-target="#drawer-view"`) || !strings.Contains(bar, `hx-swap="none"`) || !strings.Contains(bar, `"bar":"1"`) {
		t.Errorf("the bar confirm targets the drawer:\n%s", bar)
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	rec := s.As(t, s.Session(t, viewer), "GET", "/acme/shop/staging?sync=dev", nil)
	if b := rec.Body.String(); !strings.Contains(b, `id="sync-bar"`) || strings.Contains(b, "/drawer/sync/dev") {
		t.Errorf("a viewer's bar:\n%s", b)
	}
}

// Deploy queues the sync and answers with the job; it needs env.write.
func TestEnvSyncDeploy(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	ctx := context.Background()
	pl, err := s.Orch.PlanEnvSync(ctx, st.ID, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	post := "/acme/shop/staging/-/drawer/sync/dev"
	form := url.Values{"keep": {"api"}, "sig": {pl.Plan.Sig}}

	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	sess := s.Session(t, viewer)
	if rec := s.As(t, sess, "POST", post, form); rec.Code != 403 {
		t.Errorf("a viewer's deploy = %d", rec.Code)
	}
	if body := s.As(t, sess, "GET", "/acme/shop/staging/-/drawer?tab=sync&sync=dev", nil).Body.String(); strings.Contains(body, "/drawer/sync/dev") ||
		!strings.Contains(body, ">api<") {
		t.Errorf("a viewer reviews but cannot deploy:\n%s", body)
	}

	rec := s.Do(t, "POST", post, form)
	to := rec.Header().Get("HX-Redirect")
	m := jobParam.FindStringSubmatch(to)
	if rec.Code != 200 || m == nil {
		t.Fatalf("deploy = %d, redirect %q", rec.Code, to)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if j, err := s.Orch.GetJob(ctx, m[1]); err != nil || j.FinishedAt != nil {
			break
		}
	}
	if ts, err := s.Orch.Tiles(ctx, st.ID); err != nil || len(ts) != 1 || ts[0].Name != "api" {
		t.Errorf("staging tiles = %v, %v", ts, err)
	}
}

// The bar has no drawer to show a refusal in: a stale plan is a redirect to
// the review on the sync tab, as a success is a redirect to the job.
func TestEnvSyncBarRefusalRedirects(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	post := "/acme/shop/staging/-/drawer/sync/dev"
	rec := s.Do(t, "POST", post, url.Values{"keep": {"api"}, "sig": {"stale"}, "bar": {"1"}})
	want := "/acme/shop/staging?drawer=env:" + st.ID + "&tab=sync&sync=dev"
	// a dropped tile survives the refusal
	if rec := s.Do(t, "POST", post, url.Values{"keep": {"api"}, "sig": {"stale"}, "bar": {"1"}, "drop": {"api"}}); rec.Header().Get("HX-Redirect") != want+"&drop=api" {
		t.Errorf("refusal drops drop=: %q", rec.Header().Get("HX-Redirect"))
	}
	if to := rec.Header().Get("HX-Redirect"); rec.Code != 200 || to != want {
		t.Errorf("refused bar deploy = %d, redirect %q, want %q", rec.Code, to, want)
	}
	if rec.Header().Get("Set-Cookie") == "" {
		t.Error("no flash for the refusal")
	}
	pl, err := s.Orch.PlanEnvSync(context.Background(), st.ID, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = s.Do(t, "POST", post, url.Values{"keep": {"api"}, "sig": {pl.Plan.Sig}, "bar": {"1"}})
	if to := rec.Header().Get("HX-Redirect"); rec.Code != 200 || !jobParam.MatchString(to) {
		t.Errorf("bar deploy = %d, redirect %q", rec.Code, to)
	}
}

var jobParam = regexp.MustCompile(`&job=([^&]+)$`)

// Deploy ends the review: it sends the browser to the env page with no
// ?sync=, the drawer open on the job, so the tags, the ghosts and the bar
// are gone and the canvas streams again.
func TestEnvSyncDeployLeavesReview(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	pl, err := s.Orch.PlanEnvSync(context.Background(), st.ID, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := s.Do(t, "POST", "/acme/shop/staging/-/drawer/sync/dev", url.Values{"keep": {"api"}, "sig": {pl.Plan.Sig}})
	to := rec.Header().Get("HX-Redirect")
	if want := "/acme/shop/staging?drawer=env:" + st.ID + "&tab=sync&sync=dev&job="; !strings.HasPrefix(to, want) {
		t.Fatalf("redirect = %q, want %q...", to, want)
	}
	if !strings.Contains(to, "&sync=dev&job=") { // the drawer keeps its source
		t.Errorf("the redirect loses the source: %q", to)
	}
	body := s.DoNoCSRF(t, "GET", to).Body.String() // a fresh load, as the redirect makes
	for _, bad := range []string{`id="sync-bar"`, `node-id="sync:`, ">New<"} {
		if strings.Contains(body, bad) {
			t.Errorf("the page after deploy still has %q", bad)
		}
	}
	body = html.UnescapeString(body)
	for _, want := range []string{"/staging/-/jobs/", "sse-connect", "?tab=sync&sync=dev\" hx-trigger=\"sse:end"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	// another env's drawer never shows this env's job
	job := to[strings.LastIndex(to, "job=")+len("job="):]
	other := s.DoNoCSRF(t, "GET", "/acme/shop/dev?drawer=env:"+s.Tile.Env+"&tab=sync&job="+job).Body.String()
	if !strings.Contains(other, "Sync from") {
		t.Error("dev's sync tab shows staging's job, not its review")
	}
}

// The notes, the show switches and Re-arrange keep the review they sit on.
func TestEnvSyncKeepsReview(t *testing.T) {
	s := webtest.New(t)
	stagingOf(t, s)
	body := html.UnescapeString(get(t, s, "/acme/shop/staging?sync=dev&drop=api&refs=0"))
	for _, want := range []string{
		`hx-post="/acme/shop/staging/-/notes?refs=0&sync=dev&drop=api"`,
		`hx-post="/acme/shop/staging/-/reset?refs=0&sync=dev&drop=api"`,
		`hx-get="/acme/shop/staging?system=0&refs=0&sync=dev&drop=api"`, // System flipped off
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in\n%s", want, body)
		}
	}
	if body = html.UnescapeString(get(t, s, "/acme/shop/staging")); strings.Contains(body, "sync=") {
		t.Errorf("a plain page carries a review:\n%s", body)
	}
	// Re-arrange answers the canvas with the review still drawn
	rec := s.Do(t, "POST", "/acme/shop/staging/-/reset?sync=dev", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `node-id="sync:api"`) {
		t.Errorf("reset = %d, no ghost:\n%s", rec.Code, rec.Body)
	}
}

// The source picker is a full-page env URL that names the drawer's tab and
// the chosen source (the select adds sync=<slug>), so the canvas overlay
// moves with it and the drops are cleared.
func TestEnvSyncPicker(t *testing.T) {
	s := webtest.New(t)
	st := stagingOf(t, s)
	body := html.UnescapeString(get(t, s, "/acme/shop/staging/-/drawer?tab=sync&sync=dev&drop=api"))
	sel := regexp.MustCompile(`<select id="sync-from"[^>]*>`).FindString(body)
	for _, want := range []string{
		`hx-get="/acme/shop/staging?drawer=env:` + st.ID + `&tab=sync"`,
		`hx-target="#main"`,
		`hx-push-url="true"`,
		`name="sync"`,
	} {
		if !strings.Contains(sel, want) {
			t.Errorf("select lacks %q: %s", want, sel)
		}
	}
	if strings.Contains(sel, "drop") {
		t.Errorf("the picker keeps the drops: %s", sel)
	}
}
