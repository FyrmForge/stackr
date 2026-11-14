package v1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

// The extensions GET carries the daily check's findings in snake_case, and
// nothing when there are none.
func TestPGExtOutUpdates(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	v := service.PGExtView{Built: true, Updates: &service.PGExtUpdates{
		CheckedAt: at, Ref: "secret-ref",
		Changes: []service.PGExtChange{{Package: "postgresql-18-pgvector", From: "0.8.6-1", To: "0.8.7-1"}},
	}}
	b, err := json.Marshal(pgExtOut(v, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `"updates":{"checked_at":"2026-11-14T03:00:00Z","changes":[{"package":"postgresql-18-pgvector","from":"0.8.6-1","to":"0.8.7-1"}]}`
	if !strings.Contains(string(b), want) || strings.Contains(string(b), "secret-ref") {
		t.Errorf("json = %s", b)
	}
	b, _ = json.Marshal(pgExtOut(service.PGExtView{Built: true}, nil))
	if strings.Contains(string(b), "updates") {
		t.Errorf("no updates still shows: %s", b)
	}
}
