package admin

import (
	"strings"
	"testing"
)

const dbWarning = "After this, the whole control plane depends on this one disk. If the disk is missing the panel does not start, and nothing alerts you while it is down. To bring the disk back without a reboot, run stackr-install disk-check on the node."

// The rows: a button per move, "On this disk" with the way back once moved,
// the old copy with its confirmed Remove, and no box inside the card.
func TestMovesRows(t *testing.T) {
	out := render3(t, NodeDisk{State: "ready", Moves: &DiskMoves{Rows: []DiskMoveRow{
		{Name: "Logs and metrics", Button: "Move logs and metrics onto this disk", Plan: "/o/plan", Dlg: "observe-dlg"},
		{Name: "Panel database", State: "On this disk", Button: "Move back to the system disk", Plan: "/p/plan", Vals: `{"back":"1"}`, Dlg: "paneldb-dlg",
			Old: "Old copy, taken 26-11", Remove: "/p/rm", OldHint: "Deletes it."},
	}}})
	wants(t, "rows", out, ">Move logs and metrics onto this disk</button>", `hx-post="/o/plan"`, `hx-target="#observe-dlg"`,
		">Move back to the system disk</button>", ">On this disk</span>", "Old copy, taken 26-11", ">Remove old copy</button>", `hx-post="/p/rm"`, `id="paneldb-dlg"`)
	noPanel(t, "rows", out)

	busy := render3(t, NodeDisk{State: "ready", Moves: &DiskMoves{Rows: []DiskMoveRow{{Name: "Panel database", Button: "Move the panel database onto this disk", Plan: "/p/plan", Busy: true, Dlg: "paneldb-dlg"}}}})
	if !strings.Contains(busy, "disabled>Move the panel database onto this disk</button>") || strings.Contains(busy, `hx-post="/p/plan"`) {
		t.Errorf("a running move can be pressed\n%s", busy)
	}
	if none := render3(t, NodeDisk{State: "ready"}); strings.Contains(none, "Move logs and metrics") {
		t.Error("a card without moves offers them")
	}
}

// The plan dialog: the warning verbatim, the facts, the refusal as the message.
func TestMovePlanDialog(t *testing.T) {
	out := renderC(t, DiskMoveDialogView(DiskMoveDialog{
		Title: "Move the panel database onto this disk", Sub: "controller", Button: "Move to disk", Move: "/p", Dlg: "paneldb-dlg",
		Restarts: "The panel restarts.", Warning: dbWarning,
		Facts: []MoveFact{{Label: "Size", Value: "1 GB"}, {Label: "Free on the node disk", Value: "9 GB"}, {Label: "Free on the system disk", Value: "1 GB", Low: true}},
		Msg:   "Not enough room.",
	}))
	wants(t, "dialog", out, dbWarning, "Free on the node disk", "Free on the system disk", "The panel restarts.", `role="alert"`, "Not enough room.", `hx-post="/p"`)
	if !strings.Contains(out, "disabled") {
		t.Error("a refused plan leaves Move on")
	}
}
