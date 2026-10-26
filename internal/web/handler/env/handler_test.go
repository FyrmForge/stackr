package env_test

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The create dialog round-trips: the form, a refusal marking its field
// (422, the form again), then a tile the env lists and a redirect to its
// drawer.
func TestCreateTileRoundTrip(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "GET", "/acme/shop/dev/-/new-tile?source=cron&name=nightly", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="schedule"`) ||
		!strings.Contains(rec.Body.String(), `value="nightly"`) {
		t.Fatalf("form = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/new-tile", url.Values{
		"source":    {"cron"},
		"name":      {"nightly"},
		"image_ref": {"busybox:1"},
		"schedule":  {"not cron"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="create-tile"`) ||
		!strings.Contains(rec.Body.String(), `id="error-schedule"`) {
		t.Fatalf("bad schedule = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/new-tile", url.Values{
		"source":    {"cron"},
		"name":      {"nightly"},
		"image_ref": {"busybox:1"},
		"schedule":  {"0 3 * * *"},
		"command":   {"echo hi"},
	})
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("HX-Redirect"), "/acme/shop/dev?drawer=") {
		t.Fatalf("create = %d %q\n%s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	ts, err := s.Orch.Tiles(context.Background(), s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range ts {
		got = append(got, x.Name+":"+x.Kind+":"+x.Schedule+":"+x.Command)
	}
	if !strings.Contains(strings.Join(got, " "), "nightly:cron:0 3 * * *:echo hi") {
		t.Errorf("tiles after create = %v", got)
	}
}

// The volume drawer lists what backs it up; the proxy drawer the env's
// routes.
func TestVolumeAndProxyDrawers(t *testing.T) {
	s := webtest.New(t)
	v, err := s.Orch.DeclareVolume(context.Background(), service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	rec := s.Do(t, "GET", "/acme/shop/dev/-/volumes/"+v.ID+"?tab=backups", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "local disk") ||
		!strings.Contains(rec.Body.String(), "/-/volumes/"+v.ID+"/backup") {
		t.Errorf("volume drawer = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/volumes/"+v.ID+"/backup", url.Values{"method": {"volume"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "backup queued") ||
		!strings.Contains(rec.Body.String(), "?tab=backups&amp;poll=4") {
		t.Errorf("backup now = %d\n%s", rec.Code, rec.Body)
	}
	for tab, want := range map[string]string{
		"overview": "stackr-vol-" + v.ID,
		"settings": "Delete volume",
	} {
		rec = s.Do(t, "GET", "/acme/shop/dev/-/volumes/"+v.ID+"?tab="+tab, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("volume %s = %d, want %q\n%s", tab, rec.Code, want, rec.Body)
		}
	}
	rec = s.Do(t, "GET", "/acme/shop/dev/-/proxy?tab=routes", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No tile in this env has a domain.") {
		t.Errorf("proxy drawer = %d\n%s", rec.Code, rec.Body)
	}
}

// The instance drawer draws v0's four tabs; Settings is the allow list
// and the env pairs, the add input pre-filled with the org.
func TestInstanceDrawer(t *testing.T) {
	s := webtest.New(t)
	pg := managed(t, s)
	d := "/acme/shop/dev/-/instances/" + pg.Slug
	for tab, want := range map[string]string{
		"slices":   "No slice tile has been provisioned here yet.",
		"logs":     "No container is running yet.",
		"backups":  "has no volume yet",
		"settings": `name="add" value="acme:"`,
	} {
		rec := s.Do(t, "GET", d+"?tab="+tab, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s = %d, want %q\n%s", tab, rec.Code, want, rec.Body)
		}
	}
}

func managed(t *testing.T, s *webtest.Site) service.Tile {
	t.Helper()
	pg, err := s.Orch.CreateManagedTile(context.Background(), service.Tile{EnvironmentID: s.Tile.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	return pg
}

// The allow form posts the whole list; a bad pattern comes back under the
// form with what was typed (422); a member is refused the route (403).
func TestManagedAllow(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg := managed(t, s)
	d := "/acme/shop/dev/-/instances/pg/allow"
	allow := func() []string {
		m, _, err := s.Orch.InstanceSlices(ctx, pg.ID)
		if err != nil {
			t.Fatal(err)
		}
		return m.Allow
	}
	rec := s.Do(t, "POST", d, url.Values{
		"allow": {"acme:web:*"},
		"add":   {"acme:shop:*"},
	})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="allow" value="acme:shop:*"`) ||
		strings.Join(allow(), " ") != "acme:web:* acme:shop:*" {
		t.Fatalf("add = %d %v\n%s", rec.Code, allow(), rec.Body)
	}
	rec = s.Do(t, "POST", d, url.Values{
		"allow": {"acme:web:*", "acme:shop:*"},
		"add":   {"acme:x:y"},
	})
	body := rec.Body.String()
	if rec.Code != 422 || !strings.Contains(body, "Use four parts, org:stack:env:tile, or end with *.") ||
		!strings.Contains(body, `name="add" value="acme:x:y"`) || len(allow()) != 2 {
		t.Errorf("bad pattern = %d %v\n%s", rec.Code, allow(), body)
	}
	rec = s.Do(t, "POST", d, url.Values{"allow": {"acme:shop:*"}})
	if rec.Code != 200 || strings.Join(allow(), " ") != "acme:shop:*" {
		t.Errorf("remove = %d %v", rec.Code, allow())
	}

	member := s.User(t, "dev@acme.test", false)
	s.Member(t, s.Org, member, "member")
	session := s.Session(t, member)
	rec = s.As(t, session, "POST", d, url.Values{"allow": {"acme:*"}})
	if rec.Code != 403 || strings.Join(allow(), " ") != "acme:shop:*" {
		t.Errorf("member = %d %v, want 403 and the list untouched", rec.Code, allow())
	}
	rec = s.As(t, session, "GET", "/acme/shop/dev/-/instances/pg?tab=settings", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Only an owner") ||
		strings.Contains(rec.Body.String(), `hx-post="/acme/shop/dev/-/instances/pg/allow"`) {
		t.Errorf("member settings = %d\n%s", rec.Code, rec.Body)
	}
}

// The env pairs form posts the whole map; a pair to an env the stack
// lacks comes back under the form.
func TestManagedEnvPairs(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg := managed(t, s)
	d := "/acme/shop/dev/-/instances/pg/env-pairs"
	rec := s.Do(t, "POST", d, url.Values{
		"add_from": {"staging"},
		"add_to":   {"dev"},
	})
	m, _, err := s.Orch.InstanceSlices(ctx, pg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || m.EnvPairs["staging"] != "dev" || !strings.Contains(rec.Body.String(), `name="from" value="staging"`) {
		t.Errorf("add = %d %v\n%s", rec.Code, m.EnvPairs, rec.Body)
	}
	rec = s.Do(t, "POST", d, url.Values{
		"from":     {"staging"},
		"to":       {"dev"},
		"add_from": {"qa"},
		"add_to":   {"prod"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "prod is not an environment of this stack.") {
		t.Errorf("bad pair = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", d, url.Values{"from": {"staging"}})
	if rec.Code != 400 {
		t.Errorf("unpaired from = %d, want 400", rec.Code)
	}
	rec = s.Do(t, "POST", d, url.Values{"adding": {"1"}, "from": {"staging"}, "to": {"dev"}, "add_from": {""}, "add_to": {"dev"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "Name the consumer env to pair.") {
		t.Errorf("add with no consumer env = %d, want the field refused\n%s", rec.Code, rec.Body)
	}
}

// The slice drawer: its target as a link to the instance's drawer, its
// consumers with their access, the access and on-remove posts; a slice
// that resolves nowhere shows why.
func TestSliceDrawer(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg := managed(t, s)
	main, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "main", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Bound(t, main.ID, pg.ID, s.Tile.ID, "read")
	rec := s.Do(t, "GET", "/acme/shop/dev/-/slices/main", nil)
	body := rec.Body.String()
	for _, w := range []string{
		`<div id="drawer-view"`,
		`href="/acme/shop/dev?drawer=` + pg.ID + `&amp;tab=slices"`,
		"shop/dev/pg",
		s.Tile.ID + "_user",
		"slice_db",
		`bg-rw-success"></span>provisioned`,
	} {
		if rec.Code != 200 || !strings.Contains(body, w) {
			t.Errorf("slice drawer = %d, lacks %q", rec.Code, w)
		}
	}
	if strings.Contains(body, "stackr-managed") || strings.Contains(body, "provision_from") {
		t.Errorf("the overview shows a docker network or a config key:\n%s", body)
	}
	// Settings: one row each with its own Save, nothing posts on change
	body = s.Do(t, "GET", "/acme/shop/dev/-/slices/main?tab=settings", nil).Body.String()
	for _, w := range []string{`data-acc="access"`, `data-acc="consumers"`, `data-acc="remove"`, `name="consumer" value="api"`, `<option value="read" selected>`} {
		if !strings.Contains(body, w) {
			t.Errorf("slice settings lack %q", w)
		}
	}
	if _, tab, _ := strings.Cut(body, `id="tab-settings"`); strings.Contains(tab, `hx-trigger="change"`) || strings.Contains(body, "slice_access") {
		t.Errorf("slice settings autosave or name a config key:\n%s", body)
	}

	worker, err := s.Orch.CreateTile(ctx, service.Tile{
		StackID:       s.Tile.Stack,
		EnvironmentID: s.Tile.Env,
		Name:          "worker",
		Kind:          "image",
		ImageRef:      "busybox:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/slices/main/access", url.Values{
		"consumer": {"worker"},
		"access":   {"read"},
	})
	ts, err := s.Orch.Tiles(ctx, s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.ID == worker.ID && (len(x.SliceAccess) != 1 || x.SliceAccess[0].Access != "read") {
			t.Errorf("worker slice_access = %v (%d)", x.SliceAccess, rec.Code)
		}
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/slices/main/access", url.Values{"consumer": {"nope"}})
	if rec.Code != 400 {
		t.Errorf("unknown consumer = %d, want 400", rec.Code)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/slices/main/on-remove", url.Values{"on_remove": {"drop"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<option value="drop" selected>`) {
		t.Errorf("on remove = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/slices/main/default-access", url.Values{"access": {"read"}, "open": {"access"}})
	body = rec.Body.String()
	_, row, _ := strings.Cut(body, `data-acc="access"`)
	row, _, _ = strings.Cut(row, "</details>")
	if rec.Code != 200 || !strings.Contains(row, "Saved.") || !strings.Contains(body, `data-acc="access" data-changed`) {
		t.Errorf("default access = %d, the note is not in its row or the row is not changed\n%s", rec.Code, body)
	}

	if _, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "far", "shop:nope:pg", ""); err != nil {
		t.Fatal(err)
	}
	rec = s.Do(t, "GET", "/acme/shop/dev/-/slices/far", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `text-rw-danger" role="alert"`) ||
		!strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("blocked slice = %d\n%s", rec.Code, rec.Body)
	}
	// a delete leaves the drawer for the env, as a tile's does
	rec = s.Do(t, "POST", "/acme/shop/dev/-/slices/far/delete", nil)
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") != "/acme/shop/dev" {
		t.Errorf("slice delete = %d, HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}

// The create dialog makes a slice tile and opens its drawer.
func TestCreateSlice(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "GET", "/acme/shop/dev/-/new-tile?source=slice", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="provision_from"`) ||
		strings.Contains(rec.Body.String(), `name="command"`) {
		t.Fatalf("form = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/new-tile", url.Values{
		"source":         {"slice"},
		"name":           {"main"},
		"provision_from": {"shop:dev"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="error-provision_from" data-has-error="true"`) {
		t.Errorf("bad provision_from = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/new-tile", url.Values{
		"source":         {"slice"},
		"name":           {"main"},
		"provision_from": {"infra:${{ env.name }}:pg-db"},
		"default_access": {"read"},
	})
	loc := rec.Header().Get("HX-Redirect")
	if rec.Code != 200 || !strings.HasPrefix(loc, "/acme/shop/dev?drawer=") || !strings.HasSuffix(loc, "&tab=overview") {
		t.Fatalf("create = %d %q\n%s", rec.Code, loc, rec.Body)
	}
	ts, err := s.Orch.Tiles(context.Background(), s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.Slug == "main" && (x.Kind != "slice" || *x.DefaultAccess != "read") {
			t.Errorf("slice tile = %+v", x)
		}
	}
	// A config-managed stack declares its slices in the stack file.
	conn := s.Connector(t, s.Org, "whsec")
	if _, err := s.Orch.SetConfigRepo(context.Background(), s.Tile.Stack, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/new-tile", url.Values{
		"source":         {"slice"},
		"name":           {"late"},
		"provision_from": {"shop:dev:pg"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "is config-managed") ||
		!strings.Contains(rec.Body.String(), `name="provision_from"`) {
		t.Errorf("config-managed = %d\n%s", rec.Code, rec.Body)
	}
}

// A job's stream sends its rendered status, the finished state included,
// then ends.
func TestJobEvents(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	s := webtest.New(t)
	s.Healthy(s.Tile.Env)
	j, err := s.Orch.Deploy(context.Background(), s.Tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := s.DoCtx(ctx, t, "GET", "/acme/shop/dev/-/jobs/"+j.ID+"/events", nil).Body.String()
	end := strings.Index(body, "event: end")
	if end < 0 || !strings.Contains(body[:end], "event: update\ndata: <div") ||
		!strings.Contains(body[:end], ">done<") {
		t.Errorf("job events:\n%s", body)
	}
}

// A volume's schedules round-trip in the drawer: add, list, delete; a
// viewer sees no buttons and gets a 403 on post.
func TestVolumeSchedules(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	v, err := s.Orch.DeclareVolume(ctx, service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	base := "/acme/shop/dev/-/volumes/" + v.ID
	rec := s.Do(t, "POST", base+"/schedules", url.Values{
		"schedule": {"0 3 * * *"}, "tz": {"UTC"}, "method": {"volume"}, "keep": {"5"}, "mode": {"pause"},
	})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "schedule added") ||
		!strings.Contains(rec.Body.String(), "0 3 * * *") {
		t.Fatalf("add = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", base+"/schedules", url.Values{"schedule": {"nope"}, "method": {"volume"}})
	if rec.Code != 422 {
		t.Errorf("bad cron = %d, want 422", rec.Code)
	}
	ss, err := s.Orch.BackupSchedules(ctx, v.ID)
	if err != nil || len(ss) != 1 || ss[0].Keep != 5 {
		t.Fatalf("schedules = %v, %v", ss, err)
	}

	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	vs := s.Session(t, viewer)
	rec = s.As(t, vs, "GET", base+"?tab=backups", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "0 3 * * *") ||
		strings.Contains(rec.Body.String(), "/schedules") {
		t.Errorf("viewer drawer = %d\n%s", rec.Code, rec.Body)
	}
	if rec = s.As(t, vs, "POST", base+"/schedules/"+ss[0].ID+"/delete", url.Values{}); rec.Code != 403 {
		t.Errorf("viewer delete = %d, want 403", rec.Code)
	}
	if rec = s.As(t, vs, "POST", base+"/schedules", url.Values{"schedule": {"0 3 * * *"}}); rec.Code != 403 {
		t.Errorf("viewer add = %d, want 403", rec.Code)
	}

	rec = s.Do(t, "POST", base+"/schedules/"+ss[0].ID+"/delete", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "schedule deleted") {
		t.Fatalf("delete = %d\n%s", rec.Code, rec.Body)
	}
	if ss, _ = s.Orch.BackupSchedules(ctx, v.ID); len(ss) != 0 {
		t.Errorf("after delete = %v", ss)
	}
}

// An env-level drawer route loaded without htmx (its link opened in a new
// tab) sends the browser to the page that opens it, never a bare fragment.
func TestEnvDrawersLoadInShell(t *testing.T) {
	s := webtest.New(t)
	v, err := s.Orch.DeclareVolume(context.Background(), service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "uploads")
	if err != nil {
		t.Fatal(err)
	}
	env := "/acme/shop/dev"
	for path, want := range map[string]string{
		env + "/-/new-tile":                           env,
		env + "/-/proxy":                              env + "?drawer=proxy",
		env + "/-/volumes/" + v.ID:                    env + "?drawer=" + v.ID,
		env + "/-/instances/api?tab=logs":             env + "?drawer=" + s.Tile.ID + "&tab=logs",
		env + "/-/slices/api":                         env + "?drawer=" + s.Tile.ID,
		env + "/-/placement":                          env + "?drawer=env%3A" + s.Tile.Env + "&open=placement&tab=settings",
		env + "/-/tiles/api/placement":                env + "?drawer=" + s.Tile.ID + "&open=network&tab=settings",
		"/acme/shop/-/placement":                      "/acme/shop?drawer=stack%3A" + s.Tile.Stack + "&open=placement&tab=settings",
		env + "/-/drawer?tab=settings&open=placement": env + "?drawer=env%3A" + s.Tile.Env + "&open=placement&tab=settings",
		"/acme/-/placement":                           "/acme?drawer=org%3A" + s.Org + "&tab=placement",
	} {
		rec := s.DoNoCSRF(t, "GET", path)
		if rec.Code != 303 || rec.Header().Get("Location") != want {
			t.Errorf("%s = %d %q, want %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	if rec := s.Do(t, "GET", env+"/-/proxy", nil); rec.Code != 200 {
		t.Errorf("htmx proxy drawer = %d", rec.Code)
	}
}

// Deleting a database that still holds a slice is refused in its drawer,
// under the Delete row, and nothing is queued.
func TestInstanceDeleteWithSlicesRefused(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg := managed(t, s)
	main, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "main", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	s.Bound(t, main.ID, pg.ID, s.Tile.ID, "read")
	rec := s.Do(t, "POST", "/acme/shop/dev/-/instances/pg/delete", nil)
	body := rec.Body.String()
	_, row, _ := strings.Cut(body, `data-acc="delete"`)
	if rec.Code != 422 || rec.Header().Get("HX-Redirect") != "" || !strings.Contains(row, "still lives on this database") {
		t.Errorf("delete with a slice = %d, HX-Redirect %q\n%s", rec.Code, rec.Header().Get("HX-Redirect"), body)
	}
	if ts, _ := s.Orch.Tiles(ctx, s.Tile.Env); !slices.ContainsFunc(ts, func(x service.Tile) bool { return x.ID == pg.ID }) {
		t.Error("the database went")
	}
}
