package canvas

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The member names are resolved once per render and handed in: relRows maps
// every row with them and looks nothing up (a handler with no orchestrator
// would panic if it did).
func TestRelRowsUseTheNamesOfTheRender(t *testing.T) {
	h := &handler{}
	rows := []service.WindowRow{{By: "user:u1"}, {By: "user:u2"}, {By: "push"}}
	out := h.relRows(rows, map[string]string{"u1": "Ann", "u2": "Bo"}, func(service.WindowRow) rowEnv { return rowEnv{} })
	if len(out) != 3 || out[0].By == out[1].By || out[2].By != "git push" {
		t.Errorf("by = %q, %q, %q", out[0].By, out[1].By, out[2].By)
	}
}
