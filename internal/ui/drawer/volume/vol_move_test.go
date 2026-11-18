package volume

import (
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func draw(t *testing.T, comp templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := comp.Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func want(t *testing.T, where, out string, ws ...string) {
	t.Helper()
	for _, w := range ws {
		if !strings.Contains(out, w) {
			t.Errorf("%s: no %q in\n%s", where, w, out)
		}
	}
}

// 15b, 15c, 15d, 15k: the dialog's figures, and a refusal that turns the
// button off.
func TestMoveDialogStates(t *testing.T) {
	d := MoveDialog{Name: "uploads", Where: "shop / prod", Move: "/m", Need: "1.2 GB", Free: "182 GB",
		Text: "1.1 GB moves from Docker storage to the node disk. web is stopped while it copies."}
	out := draw(t, MoveDialogView(d))
	want(t, "15b", out, "<confirm-dialog open>", "Move uploads to the node disk", "uploads, shop / prod", d.Text, ">Needs</dt>", "1.2 GB", ">Free on disk</dt>", "182 GB", ">Move to disk</button>")
	if strings.Contains(out, " disabled>Move to disk</button>") || strings.Contains(out, `role="alert"`) {
		t.Errorf("a move that fits is refused\n%s", out)
	}
	d.Low, d.Free, d.Msg = true, "0.8 GB", "Not enough room. Needs 1.2 GB, 0.8 GB free."
	out = draw(t, MoveDialogView(d))
	want(t, "15c", out, "text-rw-danger", d.Msg, `role="alert"`)
	if !strings.Contains(out, " disabled>Move to disk</button>") {
		t.Errorf("Move to disk is on with no room\n%s", out)
	}
	d.Low, d.Msg = false, "A backup is running. Try again when it is done."
	out = draw(t, MoveDialogView(d))
	want(t, "15d", out, d.Msg)
	if !strings.Contains(out, " disabled>Move to disk</button>") {
		t.Error("Move to disk is on after a refusal")
	}
}

// 15e: standbys disable the button with their hint; a running move disables it
// without one; Finish move is the strip's and leaves the card clean.
func TestMoveCardStates(t *testing.T) {
	o := OverviewView{Docker: "stackr-vol-x", Storage: &Storage{Docker: true, Node: "vm-hel-2"},
		Move: &MoveView{Plan: "/p", Move: "/m", Why: "This instance has standbys. Remove them first.", Disabled: true}}
	out := draw(t, Overview(o))
	want(t, "15e", out, "This instance has standbys. Remove them first.", ">Move to disk</button>")
	if strings.Contains(out, "hx-post") {
		t.Errorf("a disabled Move to disk posts\n%s", out)
	}
	o.Move = &MoveView{Plan: "/p", Move: "/m", Finish: true, Strip: "Move did not finish. web is stopped."}
	out = draw(t, Overview(o))
	want(t, "15j", out, "Move did not finish. web is stopped.", ">Finish move</button>", `hx-post="/m"`, `role="status"`)
	if strings.Contains(out, ">Move to disk<") {
		t.Error("Move to disk beside Finish move")
	}
	jv := c.JobStatusView{Kind: "volume.to_disk", State: "failed", Error: "web did not start on the new copy."}
	o.Move, o.Job = nil, &jv
	want(t, "15h", draw(t, Overview(o)), `id="job-slot"`, "volume.to_disk", "web did not start on the new copy.")
}

// 13: both banners, and no date for a held old copy.
func TestGhostBannersAndOldCopy(t *testing.T) {
	o := OverviewView{Docker: "stackr-vol-x", Storage: &Storage{Disk: true, Docker: true, State: "ready"},
		Ghost: &GhostView{Discard: GhostDiscard("/d", "cache", "shop / prod", false, "", "")}}
	out := draw(t, Overview(o))
	want(t, "13a", out, "Two copies of this volume.", ">Discard Docker storage copy</button>", "Discard copy", `word="cache"`)
	o.Ghost = &GhostView{Bound: true, Discard: GhostDiscard("/o", "cache", "shop / prod", true, "/srv/stackr/dk_2b19/c", "/srv/stackr/dk_7f3a/c")}
	o.Storage = &Storage{Disk: true, State: "ready", Mismatch: true, Folder: "/srv/stackr/dk_2b19/c"}
	out = draw(t, Overview(o))
	want(t, "13d", out, "This volume points at the wrong folder.", ">Remove link</button>", "Mismatch", "Remove the wrong link", "Points at", "/srv/stackr/dk_2b19/c", "Should be", "/srv/stackr/dk_7f3a/c")
	o.Ghost = nil
	o.Orphaned, o.OldCopy = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), true
	out = draw(t, Overview(o))
	want(t, "old copy", out, "Old copy, kept until the move finishes.")
	if strings.Contains(out, "Orphaned since") {
		t.Error("a held old copy shows a date")
	}
}
