package admin

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

// The rows follow the service's state: the panel database row is hidden
// unless it applies (folder disk, panel not installed), on-disk rows offer the
// way back, the old copy shows DD-MM, a running move turns the button off.
func TestDiskMovesFollowTheState(t *testing.T) {
	m := diskMoves("/n", false, false, service.PanelDBView{Reason: "Only a whole disk can hold the panel database."})
	if len(m.Rows) != 1 || m.Rows[0].Button != "Move logs and metrics onto this disk" || m.Rows[0].Plan != "/n/disk/move-observe/plan" {
		t.Errorf("not applicable: %+v", m.Rows)
	}
	pv := service.PanelDBView{Applicable: true}
	m = diskMoves("/n", false, false, pv)
	if len(m.Rows) != 2 || m.Rows[1].Button != "Move the panel database onto this disk" || m.Rows[1].Old != "" {
		t.Errorf("applicable: %+v", m.Rows)
	}
	pv.OnDisk, pv.OldCopy, pv.OldCopyAt = true, true, time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	m = diskMoves("/n", true, true, pv)
	o, d := m.Rows[0], m.Rows[1]
	if o.State != "On this disk" || o.Button != "Move back to the system disk" || !o.Busy || o.Vals != `{"back":"1"}` {
		t.Errorf("observe on disk: %+v", o)
	}
	if d.State != "On this disk" || d.Old != "Old copy, taken 26-11" || d.Remove != "/n/disk/move-paneldb/remove-old" || d.Busy {
		t.Errorf("panel on disk: %+v", d)
	}
}
