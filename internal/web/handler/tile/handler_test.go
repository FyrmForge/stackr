package tile_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const drawer = "/acme/shop/dev/-/tiles/api"

// Every tab of an image tile answers a drawer fragment, never a page.
func TestEveryTabServes(t *testing.T) {
	s := webtest.New(t)
	for _, tab := range []string{
		"status",
		"logs",
		"metrics",
		"domains",
		"env",
		"access",
		"settings",
		"jobs",
		"image",
		"runs",
		"backups",
		"nonsense",
	} {
		rec := s.Do(t, "GET", drawer+"?tab="+tab, nil)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.HasPrefix(body, `<div id="drawer-view"`) || strings.Contains(body, "<html") {
			t.Errorf("tab %s = %d\n%s", tab, rec.Code, body)
		}
	}
}

// The Metrics tab writes each chart's window into the page and follows its
// tile's own stream; no chart reads the JSON API.
func TestMetricsTab(t *testing.T) {
	s := webtest.New(t)
	body := s.Do(t, "GET", drawer+"?tab=metrics&range=6h", nil).Body.String()
	for _, want := range []string{`sse-connect="` + drawer + `/metrics/events?after=`, `type="application/json"`, `range="21600"`, `hx-trigger="sse:points"`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics tab lacks %q", want)
		}
	}
	if strings.Contains(body, "/api/v1/") {
		t.Error("metrics tab points a chart at the API")
	}
}

// An action runs its verb and answers its tab; a refusal shows over the
// tab with 422 so htmx still swaps it.
func TestActions(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "POST", drawer+"/deploy", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "deploy queued") ||
		!strings.Contains(rec.Body.String(), "/-/jobs/") {
		t.Errorf("deploy = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/pause", url.Values{"paused": {"true"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "only cron tiles") {
		t.Errorf("pause an image tile = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"GREETING=hi\nNAME=bob"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `aria-label="Delete GREETING"`) ||
		!strings.Contains(rec.Body.String(), "Saved.") {
		t.Errorf("env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"drop": {"GREETING"}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Delete GREETING") ||
		!strings.Contains(rec.Body.String(), "Delete NAME") {
		t.Errorf("drop = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"NAME=bob\noops"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "Line 2 is not KEY=VALUE.") {
		t.Errorf("bad env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings?open=resources", url.Values{"fields": {"cpu_limit mem_limit_mb"}, "cpu_limit": {"lots"}, "mem_limit_mb": {"256"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="error-cpu_limit" data-has-error="true" aria-live="polite">Use a number.`) ||
		!strings.Contains(rec.Body.String(), `value="256"`) || !strings.Contains(rec.Body.String(), `value="lots"`) {
		t.Errorf("bad settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"0.5"}, "mem_limit_mb": {""}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `value="0.5"`) {
		t.Errorf("settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings?open=run", url.Values{"fields": {"command replicas restart_policy user"}, "restart_policy": {"sometimes"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="error-restart_policy" data-has-error="true"`) {
		t.Errorf("refused restart policy = %d\n%s", rec.Code, rec.Body)
	}
	// every bad field of a row at once, the service's own check among them
	rec = s.Do(t, "POST", drawer+"/settings?open=run", url.Values{"fields": {"command replicas restart_policy user shm_size_mb depends_on"},
		"replicas": {"-2"}, "shm_size_mb": {"-4"}, "depends_on": {"nosuchtile"}})
	for _, w := range []string{`id="error-replicas" data-has-error="true"`, `id="error-shm_size_mb" data-has-error="true"`, `id="error-depends_on" data-has-error="true"`} {
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), w) {
			t.Errorf("run row = %d, lacks %s\n%s", rec.Code, w, rec.Body)
		}
	}
	// a line that does not parse and a bad name in one save
	rec = s.Do(t, "POST", drawer+"/env?open=variables", url.Values{"env": {"oops\n9X=1"}})
	if body := rec.Body.String(); rec.Code != 422 || !strings.Contains(body, "Line 1 is not KEY=VALUE.") || !strings.Contains(body, "9X is not a variable name.") {
		t.Errorf("env line and name = %d\n%s", rec.Code, body)
	}
}

// Each Settings row saves on its own: a Network save keeps the limits a
// Resources save stored, answers with the Network row open, and the
// General row renames the tile.
func TestSettingsRows(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	// The fixture sets only its image and port 80.
	body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String()
	for _, row := range []string{"general", "source", "run", "storage", "health", "resources", "host"} {
		if strings.Contains(body, `data-acc="`+row+`" data-changed`) {
			t.Errorf("an untouched tile marks %s changed", row)
		}
	}
	if !strings.Contains(body, `data-acc="network" data-changed`) {
		t.Error("the port the tile sets is not marked")
	}
	rec := s.Do(t, "POST", drawer+"/settings?open=resources", url.Values{"fields": {"cpu_limit mem_limit_mb"}, "cpu_limit": {"0.5"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-acc="resources" data-changed open`) {
		t.Fatalf("resources save = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings?open=network", url.Values{"fields": {"endpoint_protocol port"}, "port": {"8080"}})
	body = rec.Body.String()
	for _, w := range []string{`data-acc="network" data-changed open`, `name="fields" value="endpoint_protocol port"`, `/placement"`, "Variables", "Database access"} {
		if rec.Code != 200 || !strings.Contains(body, w) {
			t.Errorf("network save = %d, lacks %q", rec.Code, w)
		}
	}
	ts, err := s.Orch.Tiles(ctx, s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.ID == s.Tile.ID && (x.CPULimit != 0.5 || x.ContainerPort != 8080) {
			t.Errorf("after both saves cpu %v port %d", x.CPULimit, x.ContainerPort)
		}
	}
	rec = s.Do(t, "POST", drawer+"/rename?open=general", url.Values{"name": {"web"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `/acme/shop/dev/-/tiles/web/rename`) {
		t.Errorf("rename = %d\n%s", rec.Code, rec.Body)
	}
}

// A rename onto a taken name answers under the name field, inside the
// rename form, the input marked (aria-invalid).
func TestRenameRefusedByItsForm(t *testing.T) {
	s := webtest.New(t)
	if _, err := s.Orch.CreateTile(context.Background(), service.Tile{
		StackID: s.Tile.Stack, EnvironmentID: s.Tile.Env, Name: "other", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80,
	}); err != nil {
		t.Fatal(err)
	}
	rec := s.Do(t, "POST", drawer+"/rename?open=general", url.Values{"name": {"other"}})
	body := rec.Body.String()
	form := strings.Index(body, `hx-post="`+drawer+`/rename?open=general"`)
	end := form + strings.Index(body[form:], "</form>")
	msg := strings.Index(body, "Another tile in this environment is already called other.")
	if rec.Code != 422 || form < 0 || msg < form || msg > end || strings.Count(body, "already called other") != 1 ||
		!strings.Contains(body[form:end], `aria-invalid="true"`) {
		t.Errorf("refused rename = %d, reason at %d, form ends at %d\n%s", rec.Code, msg, end, body)
	}
}

// A row save refused over a field another row holds (a volume while the
// Run row asks for 2 replicas): the reason in the Storage row, opened, the
// typed volume kept, every summary still the stored values, the Run row
// clean.
func TestSettingsRefusedInItsRow(t *testing.T) {
	s := webtest.New(t)
	if _, err := s.Orch.DeclareVolume(context.Background(), service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "data"); err != nil {
		t.Fatal(err)
	}
	rec := s.Do(t, "POST", drawer+"/settings?open=run", url.Values{"fields": {"command replicas restart_policy user"}, "replicas": {"2"}})
	if rec.Code != 200 {
		t.Fatalf("replicas = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings?open=storage", url.Values{"fields": {"files volumes"}, "volumes": {"data:/data"}})
	body := rec.Body.String()
	_, storage, _ := strings.Cut(body, `id="tile-settings-storage"`)
	storage, _, _ = strings.Cut(storage, "</details>")
	_, run, _ := strings.Cut(body, `id="tile-settings-run"`)
	run, _, _ = strings.Cut(run, "</details>")
	for _, w := range []string{`data-acc="storage" open`, `role="alert">A tile with a volume runs one replica`, `<option value="data" selected>data</option>`, `name="mount_path" value="/data"`, "no volumes · no files"} {
		if !strings.Contains(storage, w) {
			t.Errorf("storage row lacks %q\n%s", w, storage)
		}
	}
	if rec.Code != 422 || strings.Contains(run, "two writers") || strings.Contains(run, `data-acc="run" data-changed open`) || !strings.Contains(run, "2 replicas") {
		t.Errorf("run row = %d\n%s", rec.Code, run)
	}
	if strings.Contains(body, `replicas:`) || strings.Count(body, "two writers") != 1 {
		t.Errorf("the reason shows more than once or with its key\n%s", body)
	}
	// a refused domain add opens its form again with the host typed, and
	// never fills the container port beside it
	rec = s.Do(t, "POST", drawer+"/domains?open=network", url.Values{"host": {"app.example.com"}, "port": {"abc"}})
	body = rec.Body.String()
	if rec.Code != 422 || !strings.Contains(body, `<details class="mb-4" open>`) || !strings.Contains(body, `value="app.example.com"`) ||
		!strings.Contains(body, "Use a number for the port.") || strings.Contains(body, `id="set-port" name="port" value="abc"`) {
		t.Errorf("domain add = %d\n%s", rec.Code, body)
	}
}

// Writes need the CSRF token like every form on the site.
func TestActionNeedsCSRF(t *testing.T) {
	s := webtest.New(t)
	if rec := s.DoNoCSRF(t, "POST", drawer+"/deploy"); rec.Code != 403 && rec.Code != 400 {
		t.Errorf("deploy without CSRF = %d", rec.Code)
	}
}

// Add auto domain (step 7a): refused while no domain resource is visible;
// with one, the orchestrator names the host under it and records it.
func TestAutoDomain(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String(); !strings.Contains(body, `name="auto"`) {
		t.Errorf("no auto domain form:\n%s", body)
	}
	rec := s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "No domain resource to nest under") {
		t.Errorf("auto with no resource = %d\n%s", rec.Code, rec.Body)
	}
	res, err := s.Orch.CreateDomainResource(ctx, "org", s.Org, "acme.io", false, "")
	if err != nil {
		t.Fatal(err)
	}
	// the auto form has no port field: the refusal names the tile, no field
	setPort(t, s, 0)
	rec = s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "Set a container port on the tile first.") ||
		strings.Contains(rec.Body.String(), "the domain needs a container port") {
		t.Errorf("auto with no port = %d\n%s", rec.Code, rec.Body)
	}
	setPort(t, s, 80)
	rec = s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Attached api.shop.acme.io.") {
		t.Fatalf("auto = %d\n%s", rec.Code, rec.Body)
	}
	ds, err := s.Orch.Domains(ctx, s.Tile.ID)
	if err != nil || len(ds) != 1 || !ds[0].Auto || ds[0].ResourceID == nil || *ds[0].ResourceID != res.ID {
		t.Errorf("domains = %+v %v, want one auto row named by %s", ds, err, res.ID)
	}
}

func setPort(t *testing.T, s *webtest.Site, port int) {
	t.Helper()
	_, _, err := s.Orch.UpdateTile(context.Background(), s.Tile.ID, func(tl *service.Tile) error {
		tl.ContainerPort = port
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The Access tab: the slice_access entries (each a select that posts),
// the creds the tile holds, and an add form over the env's slice tiles.
func TestAccessTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg, err := s.Orch.CreateManagedTile(ctx, service.Tile{EnvironmentID: s.Tile.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	main, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "main", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "cache", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	// cache is reached by a ref alone: it shows once bound (DECIDE 204 b).
	s.Bound(t, cache.ID, pg.ID, s.Tile.ID, "write")
	rec := s.Do(t, "POST", drawer+"/access", url.Values{
		"slice":  {"main"},
		"access": {"read"},
	})
	body := rec.Body.String()
	for _, w := range []string{
		"Saved.",
		`href="/acme/shop/dev?drawer=` + main.ID + `&amp;tab=overview"`,
		`name="slice" value="main"`,
		`<option value="read" selected>`,
		`href="/acme/shop/dev?drawer=` + cache.ID + `&amp;tab=overview"`,
		s.Tile.ID + "_user",
		`<option value="cache">cache</option>`,
	} {
		if rec.Code != 200 || !strings.Contains(body, w) {
			t.Errorf("access tab = %d, lacks %q", rec.Code, w)
		}
	}
	// default drops main's entry; the add form still offers it.
	rec = s.Do(t, "POST", drawer+"/access", url.Values{
		"slice":  {"main"},
		"access": {"default"},
	})
	body = rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, `name="slice" value="main"`) || !strings.Contains(body, `<option value="main">main</option>`) {
		t.Errorf("default = %d, want main's entry gone\n%s", rec.Code, body)
	}
	rec = s.Do(t, "POST", drawer+"/access", url.Values{
		"slice":  {"nope"},
		"access": {"read"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "no slice tile nope") {
		t.Errorf("unknown slice = %d\n%s", rec.Code, rec.Body)
	}
}

// The Terminal tab needs tile.write: an owner sees it and its shell
// endpoint; a viewer neither sees the tab nor gets the tab by asking.
func TestTerminalTab(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "GET", drawer+"?tab=terminal", nil)
	body := rec.Body.String()
	for _, w := range []string{`tab=terminal`, `id="tab-terminal"`} {
		if rec.Code != 200 || !strings.Contains(body, w) {
			t.Errorf("owner terminal = %d, lacks %q\n%s", rec.Code, w, body)
		}
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	rec = s.As(t, s.Session(t, viewer), "GET", drawer+"?tab=terminal", nil)
	body = rec.Body.String()
	if rec.Code != 200 || strings.Contains(body, "terminal") {
		t.Errorf("viewer terminal = %d, want the tab absent\n%s", rec.Code, body)
	}
}

// A viewer's drawer has no write buttons; its Settings tab is the same
// rows read-only: fields disabled, nothing that posts, no Database access
// or Delete row. An owner has Deploy.
func TestViewerDrawerReadOnly(t *testing.T) {
	s := webtest.New(t)
	if body := s.Do(t, "GET", drawer, nil).Body.String(); !strings.Contains(body, "Deploy") {
		t.Fatalf("owner drawer lacks Deploy\n%s", body)
	}
	if _, err := s.Orch.AttachDomain(context.Background(), s.Tile.ID, service.DomainSpec{Host: "app.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	vs := s.Session(t, viewer)
	for _, tab := range []string{"status", "access"} {
		body := s.As(t, vs, "GET", drawer+"?tab="+tab, nil).Body.String()
		for _, w := range []string{`/deploy"`, `/restart"`, `tab=access`} {
			if strings.Contains(body, w) {
				t.Errorf("viewer tab=%s shows %q", tab, w)
			}
		}
	}
	body := s.As(t, vs, "GET", drawer+"?tab=settings", nil).Body.String()
	for _, w := range []string{`id="tab-settings"`, `data-acc="general"`, `data-acc="network"`, `data-acc="variables"`, `data-acc="dbaccess"`, "app.acme.dev", "<fieldset disabled", "Only members and owners can change"} {
		if !strings.Contains(body, w) {
			t.Errorf("viewer settings lacks %q\n%s", w, body)
		}
	}
	for _, w := range []string{"hx-post", `method="post"`, `data-acc="delete"`, "Raw Caddy", "/detach"} {
		if strings.Contains(body, w) {
			t.Errorf("viewer settings shows %q", w)
		}
	}
}

// The status tab's Access block lists the port, then each permission the
// tile holds or waits on.
func TestStatusAccess(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if _, _, err := s.Orch.UpdateTile(ctx, s.Tile.ID, func(t *service.Tile) error { t.Devices = "/dev/ttyUSB0"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Orch.Deploy(ctx, s.Tile.ID); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if g, _ := s.Orch.HostGrant(ctx, s.Tile.Stack); len(g.Pending) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	body := s.Do(t, "GET", drawer+"?tab=status", nil).Body.String()
	for _, w := range []string{">Access<", "container port", "Device", "/dev/ttyUSB0", "waiting"} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in\n%s", w, body)
		}
	}
}

// Cancel shows on a live job row for a writer, posts, and the job ends
// cancelled; a viewer sees no button and gets a 403.
func TestCancelJob(t *testing.T) {
	s := webtest.New(t)
	id := s.QueuedJob(t, s.Org, s.Tile.ID)
	cancel := drawer + "/jobs/" + id + "/cancel"
	rec := s.Do(t, "GET", drawer+"?tab=jobs", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), cancel) {
		t.Fatalf("owner jobs = %d, lacks %q\n%s", rec.Code, cancel, rec.Body)
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	vs := s.Session(t, viewer)
	rec = s.As(t, vs, "GET", drawer+"?tab=jobs", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "/cancel") {
		t.Errorf("viewer jobs = %d, want no cancel\n%s", rec.Code, rec.Body)
	}
	if rec = s.As(t, vs, "POST", cancel, url.Values{}); rec.Code != 403 {
		t.Errorf("viewer cancel = %d, want 403", rec.Code)
	}
	if rec = s.Do(t, "POST", drawer+"/jobs/nope/cancel", url.Values{}); rec.Code != 404 {
		t.Errorf("unknown job = %d, want 404", rec.Code)
	}
	rec = s.Do(t, "POST", cancel, url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "job cancelled") || strings.Contains(rec.Body.String(), "/cancel") {
		t.Fatalf("cancel = %d\n%s", rec.Code, rec.Body)
	}
	j, err := s.Orch.GetJob(context.Background(), id)
	if err != nil || j.State != "cancelled" {
		t.Errorf("job = %q, %v", j.State, err)
	}
}

// A job past the newest 20 still cancels; one that finished meanwhile shows
// the conflict, not "job cancelled"; another tile's job is a 404.
func TestCancelJobLookup(t *testing.T) {
	s := webtest.New(t)
	old := s.QueuedJob(t, s.Org, s.Tile.ID)
	for range 25 {
		s.QueuedJob(t, s.Org, s.Tile.ID)
	}
	rec := s.Do(t, "POST", drawer+"/jobs/"+old+"/cancel", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "job cancelled") {
		t.Fatalf("old job cancel = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/jobs/"+old+"/cancel", url.Values{})
	if !strings.Contains(rec.Body.String(), "the job already finished") || strings.Contains(rec.Body.String(), "job cancelled") {
		t.Errorf("finished job cancel = %d\n%s", rec.Code, rec.Body)
	}
	other := s.QueuedJob(t, s.Org, "some-other-tile")
	if rec = s.Do(t, "POST", drawer+"/jobs/"+other+"/cancel", url.Values{}); rec.Code != 404 {
		t.Errorf("other tile's job = %d, want 404", rec.Code)
	}
}

// Storage's volume rows fold into the volumes lines; a volume the env does
// not declare is refused on the field, the rows kept as typed, and the row
// links to the env's Volumes to declare one.
func TestStorageMounts(t *testing.T) {
	s := webtest.New(t)
	if _, err := s.Orch.DeclareVolume(context.Background(), service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "uploads"); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"fields":       {"files volumes"},
		"mount_volume": {"uploads", "ghost", ""},
		"mount_path":   {"/data", "/ghost", ""},
		"mount_mode":   {"ro", "", ""},
		"mount_other":  {""},
	}
	rec := s.Do(t, "POST", drawer+"/settings?open=storage", form)
	b := rec.Body.String()
	for _, w := range []string{
		"does not declare ghost",
		`<option value="ghost" selected>ghost</option>`,
		`name="mount_path" value="/ghost"`,
		`aria-invalid="true"`,
		`?drawer=env:` + s.Tile.Env + `&amp;tab=settings&amp;open=volumes`,
	} {
		if !strings.Contains(b, w) {
			t.Errorf("refused mounts lack %q\n%s", w, b)
		}
	}
	if rec.Code != 422 {
		t.Errorf("undeclared = %d", rec.Code)
	}
	form["mount_volume"], form["mount_path"], form["mount_mode"] = []string{"uploads", ""}, []string{"/data", ""}, []string{"ro", ""}
	form["mount_other"] = []string{"host:/srv/x:/x"}
	if rec := s.Do(t, "POST", drawer+"/settings?open=storage", form); rec.Code != 200 {
		t.Fatalf("declared = %d\n%s", rec.Code, rec.Body)
	}
	ts, err := s.Orch.Tiles(context.Background(), s.Tile.Env)
	if err != nil || len(ts) != 1 || ts[0].Volumes != "uploads:/data:ro\nhost:/srv/x:/x" {
		t.Errorf("stored = %+v %v", ts, err)
	}
}
