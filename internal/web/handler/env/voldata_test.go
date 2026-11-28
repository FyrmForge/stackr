package env_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Wipe, delete and restore of a volume in its drawer.

func dataSite(t *testing.T) (s *webtest.Site, root, base, vid string) {
	t.Helper()
	s = webtest.New(t)
	root = s.Session(t, s.User(t, "root@x.test", true))
	vid = s.Volume(t, "env", s.Tile.Env, "pgdata")
	return s, root, "/acme/shop/dev/-/volumes/" + vid, vid
}

func TestVolumeNoBackupOnOverview(t *testing.T) {
	s, root, base, _ := dataSite(t)
	hasAll(t, "no backup", s.As(t, root, "GET", base, nil).Body.String(), "No backup", "Set up backup", `hx-get="`+base+`?tab=backups"`)
}

func TestVolumeSettingsOffersWipeAndDelete(t *testing.T) {
	s, root, base, _ := dataSite(t)
	hasAll(t, "settings", s.As(t, root, "GET", base+"?tab=settings", nil).Body.String(),
		"Wipe contents", base+"/wipe", "Stop tiles using it", "Back up first", "Backs up to the local disk first.",
		"Deleting this volume removes it and its data at once.", base+"/delete")
}

func TestVolumeWipeQueuesAJob(t *testing.T) {
	s, root, base, vid := dataSite(t)
	rec := s.As(t, root, "POST", base+"/wipe", url.Values{"stop": {"on"}, "backup": {"on"}})
	if rec.Code != 200 {
		t.Fatalf("wipe = %d\n%s", rec.Code, rec.Body)
	}
	hasAll(t, "wipe job", rec.Body.String(), "volume.wipe")
	if _, ok, err := s.Orch.VolumeDataJob(t.Context(), vid); err != nil || !ok {
		t.Errorf("no wipe job row: %v %v", ok, err)
	}
}

// A refused wipe (a removed volume) shows the reason on the Settings tab.
func TestVolumeWipeRefusalShown(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	at := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	create(t, s.Store.Volumes.Create, map[string]any{"ID": "v-gone", "ScopeKind": "env", "ScopeID": s.Tile.Env, "Slug": "gone", "Name": "gone", "CreatedAt": time.Now(), "OrphanedAt": &at})
	rec := s.As(t, root, "POST", "/acme/shop/dev/-/volumes/v-gone/wipe", nil)
	if rec.Code != 422 {
		t.Fatalf("wipe of a removed volume = %d", rec.Code)
	}
	hasAll(t, "refusal", rec.Body.String(), "is removed. Restore it first.")
}

func TestVolumeDeleteAtOnceClosesTheDrawer(t *testing.T) {
	s, root, base, vid := dataSite(t)
	rec := s.As(t, root, "POST", base+"/delete", nil)
	if rec.Code != 200 || rec.Header().Get("HX-Redirect") != "/acme/shop/dev" {
		t.Fatalf("delete = %d redirect %q\n%s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	if _, err := s.Orch.Volume(t.Context(), vid); err == nil {
		t.Error("the volume is still there")
	}
	// the drawer left open on it closes too
	if rec := s.As(t, root, "GET", base, url.Values{}); rec.Code == 200 && !strings.Contains(rec.Header().Get("HX-Redirect"), "/acme/shop/dev") {
		t.Errorf("a gone volume's drawer stays open\n%s", rec.Body)
	}
}

func TestVolumeDeleteWithBackupShowsTheJob(t *testing.T) {
	s, root, base, _ := dataSite(t)
	rec := s.As(t, root, "POST", base+"/delete", url.Values{"backup": {"on"}})
	if rec.Code != 200 {
		t.Fatalf("delete = %d\n%s", rec.Code, rec.Body)
	}
	hasAll(t, "delete job", rec.Body.String(), "volume.delete")
}

// A backup-first delete that failed leaves the volume and says so in the drawer.
func TestVolumeDeleteFailureShown(t *testing.T) {
	s, root, base, vid := dataSite(t)
	now := time.Now()
	create(t, s.Store.Jobs.Create, map[string]any{"ID": "jd", "Kind": "volume-delete", "State": "failed",
		"Payload": `{"volume_id":"` + vid + `","backup_first":true}`, "Error": "backup before the delete failed, nothing was deleted: dest down",
		"CreatedAt": now, "FinishedAt": &now})
	hasAll(t, "overview", s.As(t, root, "GET", base, nil).Body.String(), "volume.delete", "failed", "dest down", "Nothing was deleted.")
}

func TestRemovedVolumeRestores(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	at := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	create(t, s.Store.Volumes.Create, map[string]any{"ID": "v-gone", "ScopeKind": "env", "ScopeID": s.Tile.Env, "Slug": "gone", "Name": "gone", "CreatedAt": time.Now(), "OrphanedAt": &at})
	base := "/acme/shop/dev/-/volumes/v-gone"
	settings := s.As(t, root, "GET", base+"?tab=settings", nil).Body.String()
	hasAll(t, "removed settings", settings, "Restore", base+"/restore-volume")
	if strings.Contains(settings, "Delete volume") || strings.Contains(settings, "Wipe contents") {
		t.Errorf("a removed volume offers delete or wipe\n%s", settings)
	}
	if strings.Contains(s.As(t, root, "GET", base, nil).Body.String(), "Set up backup") {
		t.Error("a removed volume offers backup setup")
	}
	rec := s.As(t, root, "POST", base+"/restore-volume", nil)
	if rec.Code != 200 {
		t.Fatalf("restore = %d\n%s", rec.Code, rec.Body)
	}
	hasAll(t, "restored", rec.Body.String(), "Volume restored.")
	if v, err := s.Orch.Volume(t.Context(), "v-gone"); err != nil || v.OrphanedAt != nil {
		t.Errorf("still removed: %v %v", v.OrphanedAt, err)
	}
}
