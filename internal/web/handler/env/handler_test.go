package env_test

import (
	"context"
	"net/url"
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
	v, err := s.Orch.DeclareVolume(context.Background(), service.VolumeScope{Kind: "env", ID: s.Tile.Env}, "uploads", 0)
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

// The instance drawer draws v0's four tabs; a scope save answers
// Settings with the new chip, a bad scope is refused inline.
func TestInstanceDrawer(t *testing.T) {
	s := webtest.New(t)
	pg, err := s.Orch.CreateManagedTile(context.Background(), service.Tile{EnvironmentID: s.Tile.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	d := "/acme/shop/dev/-/instances/" + pg.Slug
	for tab, want := range map[string]string{
		"slices":   "No tile has a slice of it yet.",
		"logs":     "No container is running yet.",
		"backups":  "has no volume yet",
		"settings": `name="scope_kind"`,
	} {
		rec := s.Do(t, "GET", d+"?tab="+tab, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Body.String(), "env-scoped") {
			t.Errorf("%s = %d, want %q\n%s", tab, rec.Code, want, rec.Body)
		}
	}
	rec := s.Do(t, "POST", d+"/scope", url.Values{"scope_kind": {"stack"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "scope saved") || !strings.Contains(rec.Body.String(), "stack-scoped") {
		t.Errorf("scope = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", d+"/scope", url.Values{"scope_kind": {"galaxy"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "banner-danger") {
		t.Errorf("bad scope = %d\n%s", rec.Code, rec.Body)
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
