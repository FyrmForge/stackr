package env_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The volume's size cap (mockup 19): Overview shows it, Settings edits it. A
// change only stages it; Apply saves it.

func capSite(t *testing.T) (s *webtest.Site, root, base, vid string) {
	t.Helper()
	s = webtest.New(t)
	root = s.Session(t, s.User(t, "root@x.test", true))
	vid = s.Volume(t, "env", s.Tile.Env, "pgdata")
	return s, root, "/acme/shop/dev/-/volumes/" + vid, vid
}

func savedCap(t *testing.T, s *webtest.Site, vid string) *int64 {
	t.Helper()
	v, err := s.Orch.Volume(t.Context(), vid)
	mustf(t, err)
	return v.MaxSizeMB
}

// 19a, 19b, 19q: Overview shows no cap, then the cap, and says a cap in Docker
// storage is not enforced; the link opens Settings.
func TestCapFactOnOverview(t *testing.T) {
	s, root, base, vid := capSite(t)
	body := s.As(t, root, "GET", base, nil).Body.String()
	hasAll(t, "19a", body, "Size cap", "No cap", "Edit in Settings", `hx-get="`+base+`?tab=settings"`)
	mb := int64(20 << 10)
	mustf(t, s.Orch.SetVolumeCap(t.Context(), vid, &mb))
	body = s.As(t, root, "GET", base, nil).Body.String()
	hasAll(t, "19q", body, "20 GB", "Not enforced in Docker storage.")
}

// 19m: on a folder disk the fact carries the permanent not-enforced line.
func TestCapFactOnAFolderDisk(t *testing.T) {
	s, root, base, vid := capSite(t)
	_, diskID := disk(t, s, root)
	v, err := s.Orch.Volume(t.Context(), vid)
	mustf(t, err)
	folder(t, s, diskID, v.Name)
	mb := int64(1500)
	mustf(t, s.Orch.SetVolumeCap(t.Context(), vid, &mb))
	body := s.As(t, root, "GET", base, nil).Body.String()
	hasAll(t, "19m", body, "1500 MB", "Not enforced on a folder disk.")
	hasAll(t, "19o", s.As(t, root, "GET", base+"?tab=settings", nil).Body.String(), "Not enforced on a folder disk.")
}

// 19e to 19l: the pencil opens the field, a change stages the cap (nothing is
// saved), a refused size keeps the field open, Apply saves, empty removes.
func TestCapSettingsFlow(t *testing.T) {
	s, root, base, vid := capSite(t)
	mb := int64(20 << 10)
	mustf(t, s.Orch.SetVolumeCap(t.Context(), vid, &mb))
	get := func(q string) string { return s.As(t, root, "GET", base+q, nil).Body.String() }
	stage := func(size, unit string) string {
		t.Helper()
		rec := s.As(t, root, "POST", base+"/cap/stage", url.Values{"size": {size}, "unit": {unit}})
		if rec.Code != 200 {
			t.Fatalf("stage %s %s = %d\n%s", size, unit, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	apply := func(v string) string {
		t.Helper()
		return s.As(t, root, "POST", base+"/cap", url.Values{"max_size_mb": {v}}).Body.String()
	}

	hasAll(t, "19e", get("?tab=settings"), "Size cap", "20 GB", `aria-label="Edit size cap"`)
	hasAll(t, "19f", get("?tab=settings&cap=edit"), `name="size"`, `value="20"`, `<option value="GB" selected>`, "Leave empty for no cap.", `hx-post="`+base+`/cap/stage"`)

	// staged: the footer says what changes and that it applies at once; nothing is saved
	hasAll(t, "19g", stage("30", "GB"), "1 change not saved", "20 GB", "30 GB", "Saved, not enforced in Docker storage.", `hx-post="`+base+`/cap"`, "&#34;max_size_mb&#34;:&#34;30720&#34;")
	if got := savedCap(t, s, vid); got == nil || *got != 20<<10 {
		t.Fatalf("staging saved %v", got)
	}
	// the same value as saved stages nothing
	if body := stage("20", "GB"); strings.Contains(body, "1 change not saved") {
		t.Error("an unchanged cap shows a footer")
	}
	// refused sizes keep the field open with the service's words
	hasAll(t, "19j", stage("0", "MB"), `aria-invalid="true"`, "Give a size of at least 1 MB, or leave it empty for no cap.")
	hasAll(t, "19k", stage("2000000000", "GB"), `aria-invalid="true"`, "Give a size up to 1 PB.")
	hasAll(t, "word", stage("abc", "MB"), "Give a size of at least 1 MB, or leave it empty for no cap.")
	// empty stages the removal
	hasAll(t, "19l", stage("", "GB"), "1 change not saved", "No cap", "&#34;max_size_mb&#34;:&#34;&#34;")

	// Apply saves; the answer is the Settings tab with the note
	hasAll(t, "applied", apply("30720"), "Size cap set.", "30 GB")
	if got := savedCap(t, s, vid); got == nil || *got != 30<<10 {
		t.Fatalf("applied cap = %v", got)
	}
	hasAll(t, "removed", apply(""), "Size cap removed.", "No cap")
	if got := savedCap(t, s, vid); got != nil {
		t.Fatalf("cap after removal = %v", *got)
	}
	// a refused Apply keeps the staged cap, shows the reason, changes nothing
	mustf(t, s.Orch.SetVolumeCap(t.Context(), vid, &mb))
	hasAll(t, "19i", apply("0"), "1 change not saved", "Give a size of at least 1 MB, or leave it empty for no cap.")
	if got := savedCap(t, s, vid); got == nil || *got != 20<<10 {
		t.Fatalf("a refused apply saved %v", got)
	}
}

// Only a person who may write tiles sees the pencil; the routes need it too.
func TestCapNeedsWriteAccess(t *testing.T) {
	s, _, base, vid := capSite(t)
	anon := s.As(t, "", "POST", base+"/cap", url.Values{"max_size_mb": {"100"}})
	if anon.Code == 200 {
		t.Errorf("anonymous Apply = %d", anon.Code)
	}
	if got := savedCap(t, s, vid); got != nil {
		t.Errorf("anonymous Apply saved %v", *got)
	}
}
