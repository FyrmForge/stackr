package env

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// A standby may be removed unless it is already going; only one that needs a
// rebuild or rewind gets Rebuild; the primary gets neither.
func TestMemberRowActions(t *testing.T) {
	rows := memberRows([]service.StandbyView{
		{ID: "p", Role: "primary", State: "running"},
		{ID: "a", Role: "replica", State: "streaming"},
		{ID: "b", Role: "replica", State: "needs rebuild"},
		{ID: "c", Role: "replica", State: "fenced, needs rewind"},
		{ID: "d", Role: "replica", State: "removing"},
	})
	want := map[string][2]bool{"p": {false, false}, "a": {true, false}, "b": {true, true}, "c": {true, true}, "d": {false, false}}
	for _, r := range rows {
		if got := [2]bool{r.CanRemove, r.CanRebuild}; got != want[r.ID] {
			t.Errorf("%s: remove/rebuild = %v, want %v", r.ID, got, want[r.ID])
		}
	}
}
