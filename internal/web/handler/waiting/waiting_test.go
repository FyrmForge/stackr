package waiting_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The page names what waits on the owner and the rail carries the count;
// the banners poll swaps that count in place.
func TestWaitingPage(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "GET", "/-/waiting", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nothing is waiting on you.") {
		t.Fatalf("empty = %d\n%s", rec.Code, rec.Body)
	}
	s.WaitingJob(t, "gate.password", `{"tile_id":"`+s.Tile.ID+`"}`)
	body := s.Do(t, "GET", "/-/waiting", nil).Body.String()
	for _, want := range []string{"Values to set", ", gate.password", "waits", ">Set<", `id="rail-you"`, `hx-select-oob="#rail-you"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if poll := s.Do(t, "GET", "/-/banners", nil).Body.String(); !strings.Contains(poll, `hx-swap-oob="true">1</span>`) {
		t.Errorf("banners poll lacks the count:\n%s", poll)
	}
}
