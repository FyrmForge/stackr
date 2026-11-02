package env_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A job has a log page under its env, and the stream's status line links it.
func TestJobLogPage(t *testing.T) {
	s := webtest.New(t)
	j, err := s.Orch.Deploy(t.Context(), s.Tile.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/acme/shop/dev/-/jobs/" + j.ID
	rec := s.Do(t, "GET", path, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="job-log"`) {
		t.Fatalf("log page = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.Do(t, "GET", path+"/log?offset=0", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="job-head"`) {
		t.Fatalf("log poll = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.Do(t, "GET", "/acme/shop/dev/-/jobs/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown job = %d, want 404", rec.Code)
	}
}
