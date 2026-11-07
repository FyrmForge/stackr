package env_test

import (
	"net/url"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The service's refusals reach the panel: a window out of range, a number
// that is not one, and a restore time with no archive behind it.
func TestPITRFormErrors(t *testing.T) {
	s := webtest.New(t)
	pg := managed(t, s)
	d := "/acme/shop/dev/-/instances/" + pg.Slug
	if rec := s.Do(t, "POST", d+"/pitr", url.Values{"days": {"abc"}}); rec.Code != 400 {
		t.Errorf("days=abc = %d, want 400", rec.Code)
	}
	rec := s.Do(t, "POST", d+"/pitr", url.Values{"days": {"999"}})
	if rec.Code < 400 || rec.Code == 500 {
		t.Errorf("days=999 = %d, want a refusal\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", d+"/restore", url.Values{"time": {"2026-11-07T10:42"}})
	if rec.Code < 400 || rec.Code == 500 {
		t.Errorf("restore with no archive = %d, want a refusal\n%s", rec.Code, rec.Body)
	}
}
