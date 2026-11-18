package admin

import (
	"strings"
	"testing"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// The phase 2 dialogs, drawn from constructed views: the states the handler
// tests cannot reach (a worker's disk facts, standbys) and the exact words.

func renderC(t *testing.T, comp templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := comp.Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func wants(t *testing.T, where, out string, ws ...string) {
	t.Helper()
	for _, w := range ws {
		if !strings.Contains(out, w) {
			t.Errorf("%s: no %q in\n%s", where, w, out)
		}
	}
}

func noPanel(t *testing.T, where, out string) {
	t.Helper()
	if strings.Contains(out, `class="panel`) || strings.Contains(out, "rounded-lg border") {
		t.Errorf("%s: a bordered box in a card\n%s", where, out)
	}
}

// 12h: a refusal known on open replaces the typed field and leaves Close only.
func TestForgetDialogRefusedOnOpenHasCloseOnly(t *testing.T) {
	f := DiskForget{
		Action: "/x", Vals: "{}", Node: "vm-hel-2", Disk: "dk_7f3a", Word: "vm-hel-2",
		Volumes: []ForgetVolume{{Name: "pgdata", Where: "shop / prod", Note: "backup of Nov 17"}, {Name: "redis", Where: "shop / staging", Note: "starts empty", Empty: true}},
		Refusal: "Promote or remove the standby of shop / prod postgres first.",
	}
	out := renderC(t, forgetDialog(f, false))
	wants(t, "12h", out, "Forget this disk", "vm-hel-2, disk dk_7f3a", "Its volumes come back from their newest backup. Tiles that use them restart.",
		"Volumes without a backup start empty.", "backup of Nov 17", "starts empty", "Promote or remove the standby of shop / prod postgres first.", `role="alert"`, ">Close</button>")
	for _, no := range []string{"data-word", "data-confirm", "Type "} {
		if strings.Contains(out, no) {
			t.Errorf("a refused forget carries %q\n%s", no, out)
		}
	}
	noPanel(t, "12h", out)

	// 12b2: an adding disk has no volumes: no list, and the typed field is there
	f.Refusal, f.Volumes = "", nil
	out = renderC(t, forgetDialog(f, false))
	wants(t, "12b2", out, `word="vm-hel-2"`, "data-confirm")
	if strings.Contains(out, "Volumes without") || strings.Contains(out, ">Volumes</h3>") {
		t.Errorf("a list with no volumes\n%s", out)
	}

	// 12i, 12j: the record of a forgotten node's disk
	g := DiskForget{Action: "/x", Vals: "{}", Node: "vm-hel-9", Disk: "dk_91bc", Word: "dk_91bc", Gone: true, Refusal: "shop / prod, shop / staging and 1 more still point at vm-hel-9. Recover them first."}
	out = renderC(t, forgetDialog(g, true))
	wants(t, "12i", out, `class="link`, ">Forget record</button>", "Forget this disk record", "dk_91bc, was on vm-hel-9", "The record of this disk is removed. Nothing else changes.", "Recover them first.", ">Close</button>")
	g.Refusal = ""
	out = renderC(t, forgetDialog(g, true))
	wants(t, "12j", out, `word="dk_91bc"`, "data-confirm", ">Forget record</button>")
}

// 14b, 14f: step one, with and without a refusal under the folder.
func TestAdoptStepOneStates(t *testing.T) {
	a := AdoptDialog{PlanURL: "/p", Adopt: "/a", Path: "/srv/stackr",
		Cands: []AdoptCand{{Disk: "dk_91bc", Was: "vm-hel-9", Forget: DiskForget{Action: "/f", Vals: "{}", Disk: "dk_91bc", Word: "dk_91bc", Gone: true, Node: "vm-hel-9"}}}}
	out := renderC(t, AdoptDialogView(a))
	wants(t, "14b", out, `id="adopt-dlg"`, "Adopt a disk", "Take over a disk whose node is gone. Its volumes stay on it.", `value="/srv/stackr"`, `placeholder="/mnt/disk"`,
		"The mounted folder of a disk from another node.", "Reading the disk...", "Disks of forgotten nodes · 1", "dk_91bc", "was vm-hel-9", "Forget record")
	if strings.Contains(out, "aria-invalid") {
		t.Error("an invalid field with no refusal")
	}
	a.Msg = "vm-hel-1 is still online. Forget the node first."
	out = renderC(t, AdoptDialogView(a))
	wants(t, "14f", out, `aria-invalid="true"`, "!border-rw-danger", a.Msg, `role="alert"`)
	noPanel(t, "14b", out)
}

// 14c, 14d: the plan lists what moves; a copy in the way turns Adopt disk off
// and carries its Discard.
func TestAdoptPlanLists(t *testing.T) {
	v := &AdoptPlanView{
		Sub: "/srv/stackr, from vm-hel-1 (gone)", Node: "vm-hel-5", Vals: "{}",
		Envs:      []DlgPair{{Name: "shop / prod", Where: "3 tiles", Note: "moves to vm-hel-5"}},
		OnDisk:    []DlgPair{{Name: "pgdata", Where: "shop / prod", Note: "stays on the disk"}},
		Elsewhere: []DlgPair{{Name: "redis", Where: "shop / prod", Note: "restored from backup of Nov 14"}, {Name: "scratch", Where: "shop / staging", Note: "starts empty", Warn: true}},
		Unclaimed: []string{"old-test"},
	}
	a := AdoptDialog{PlanURL: "/p", Adopt: "/a", View: v}
	out := renderC(t, AdoptDialogView(a))
	wants(t, "14c", out, "Adopt this disk", v.Sub, ">Envs</h3>", "3 tiles", "moves to vm-hel-5", "Volumes on the disk · 1", "Volumes not on the disk · 2",
		"restored from backup of Nov 14", "text-rw-warn", "starts empty", "Unclaimed folders · 1", "no volume", "kept on the disk", "Unclaimed folders are kept.")
	noPanel(t, "14c", out)

	v.Copies = []AdoptCopyRow{{Name: "cache", Where: "shop / prod", Note: "copy in Docker storage",
		Discard: c.DiskConfirmView{Opener: "Discard", Link: true, Title: "Discard Docker storage copy", Sub: "cache, shop / prod, on vm-hel-5", Word: "cache", Button: "Discard copy", Action: "/d", Target: "#adopt-dlg", Body: TextBody("x")}}}
	out = renderC(t, AdoptDialogView(a))
	wants(t, "14d", out, "Already on vm-hel-5 · 1", "text-rw-danger", "copy in Docker storage", ">Discard</button>", "Discard this copy to adopt.", `word="cache"`, "cache, shop / prod, on vm-hel-5")
	if !strings.Contains(out, " disabled>Adopt disk</button>") {
		t.Errorf("Adopt disk is on with a copy in the way\n%s", out)
	}
}

// 16b, 16c: the groups, the skipped and the room; no room turns the button off.
func TestMoveAllPlanDialog(t *testing.T) {
	m := MoveAllDialog{
		Move: "/m", Node: "vm-hel-2", Count: 3, Need: "12 GB", Free: "182 GB",
		Groups: []MoveGroup{
			{Title: "Group 1 · restarts web", Rows: []MoveRow{{Name: "uploads", Where: "shop / prod", Note: "1.1 GB · will move"}}},
			{Title: "Group 2 · restarts postgres", Rows: []MoveRow{{Name: "pgdata", Where: "shop / prod", Note: "4.2 GB · finishes an earlier move"}}},
		},
		Skipped: []MoveRow{{Name: "archive", Where: "shop / prod", Note: "Does not fit on the disk."}},
	}
	out := renderC(t, MoveAllDialogView(m))
	wants(t, "16b", out, "Move all to disk", "3 volumes on vm-hel-2", "Each group copies in turn. Only its own tiles are stopped.", "Group 1 · restarts web",
		"1.1 GB · will move", "Group 2 · restarts postgres", "4.2 GB · finishes an earlier move", ">Skipped</h3>", "Does not fit on the disk.", "12 GB", "182 GB", ">Free on disk</dt>")
	if strings.Contains(out, " disabled>Move all to disk</button>") || strings.Contains(out, `role="alert"`) {
		t.Errorf("a plan that fits is refused\n%s", out)
	}
	m.Low, m.Free = true, "8 GB"
	out = renderC(t, MoveAllDialogView(m))
	wants(t, "16c", out, "Not enough room. Needs 12 GB, 8 GB free.", "text-rw-danger", `role="alert"`)
	if !strings.Contains(out, " disabled>Move all to disk</button>") {
		t.Errorf("Move all is on with no room\n%s", out)
	}
	noPanel(t, "16b", out)
}

// 16e, 16f: one line per job, with its state; a failed job says why.
func TestMoveJobsList(t *testing.T) {
	d := NodeDisk{MoveJobs: []MoveJobRow{
		{Names: "uploads", State: "done", Href: "/-/jobs/1"},
		{Names: "pgdata", State: "failed", Msg: "The copy failed. The volume is unchanged.", Failed: true, Href: "/-/jobs/2"},
		{Names: "cache, media", State: "running", Href: "/-/jobs/3"},
	}}
	out := renderC(t, moveJobs(d))
	wants(t, "16f", out, "volume.to_disk", ">uploads</span>", ">cache, media</span>", ">failed<", ">running<", ">done<", "text-rw-danger", "The copy failed. The volume is unchanged.", ">details<")
	if strings.Count(out, "<li ") != 3 {
		t.Errorf("one line per job: %s", out)
	}
	noPanel(t, "16f", out)
}

// 12f, 14l: the strip of an interrupted forget or adopt offers Resume only.
func TestInterruptedStripOffersResume(t *testing.T) {
	d := NodeDisk{Has: true, State: "forgetting", Resume: &DiskResume{Action: "/r", Vals: `{"disk":"d"}`, Text: "Forgetting did not finish."}}
	out := renderC(t, diskStrip(d))
	wants(t, "12f", out, "Forgetting did not finish.", ">Resume</button>", `hx-post="/r"`, `role="status"`)
	if strings.Contains(out, "Check disk") {
		t.Error("Check disk beside Resume")
	}
	d.Resume.Text = "Adopting did not finish."
	wants(t, "14l", renderC(t, diskStrip(d)), "Adopting did not finish.", ">Resume</button>")
}
