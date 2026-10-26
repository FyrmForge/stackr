package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The env Settings Volumes row: Add declares one (a bad name refused on
// its field, the row open again), the list names who mounts it, Remove is
// refused while a tile mounts it and orphans it after; a viewer only reads.
func TestEnvVolumesRow(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	base := "/acme/shop/dev/-/drawer"
	if body := get(t, s, base+"?tab=settings"); !strings.Contains(body, `data-acc="volumes">`) || !strings.Contains(body, "None</span>") {
		t.Fatalf("no empty Volumes row:\n%s", body)
	}

	rec := s.Do(t, "POST", base+"/volumes", url.Values{"volume": {"Bad Name"}, "open": {"volumes"}})
	b := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(b, `data-acc="volumes" open`) ||
		!strings.Contains(b, `value="Bad Name"`) || !strings.Contains(b, `aria-invalid="true"`) || !strings.Contains(b, "lower-case letters") {
		t.Fatalf("bad name = %d\n%s", rec.Code, b)
	}

	rec = s.Do(t, "POST", base+"/volumes", url.Values{"volume": {"uploads"}, "open": {"volumes"}})
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, "Added uploads.") || !strings.Contains(b, "1 volume</span>") {
		t.Fatalf("add = %d\n%s", rec.Code, b)
	}
	if _, _, err := s.Orch.UpdateTile(ctx, s.Tile.ID, func(t *service.Tile) error { t.Volumes = "uploads:/data"; return nil }); err != nil {
		t.Fatal(err)
	}
	vs, err := s.Orch.ListEnvVolumes(ctx, s.Tile.Env)
	if err != nil || len(vs.Volumes) != 1 {
		t.Fatalf("volumes = %+v %v", vs, err)
	}
	remove := base + "/volumes/" + vs.Volumes[0].ID + "/remove"
	body := get(t, s, base+"?tab=settings&open=volumes")
	if !strings.Contains(body, "mounted by api") || !strings.Contains(body, `hx-post="`+remove+`"`) {
		t.Errorf("list:\n%s", body)
	}

	rec = s.Do(t, "POST", remove, url.Values{"open": {"volumes"}})
	if b := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(b, "mounted by api") {
		t.Errorf("remove in use = %d\n%s", rec.Code, b)
	}

	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	vb := s.As(t, s.Session(t, viewer), "GET", base+"?tab=settings&open=volumes", nil).Body.String()
	if strings.Contains(vb, base+"/volumes") || !strings.Contains(vb, "Only members and owners can change volumes.") {
		t.Errorf("a viewer can change volumes:\n%s", vb)
	}
	if rec := s.As(t, s.Session(t, viewer), "POST", base+"/volumes", url.Values{"volume": {"x"}}); rec.Code != http.StatusForbidden {
		t.Errorf("viewer add = %d", rec.Code)
	}

	if _, _, err := s.Orch.UpdateTile(ctx, s.Tile.ID, func(t *service.Tile) error { t.Volumes = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	rec = s.Do(t, "POST", remove, url.Values{"open": {"volumes"}})
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, "data is kept") || !strings.Contains(b, "None</span>") {
		t.Errorf("remove = %d\n%s", rec.Code, b)
	}
}
