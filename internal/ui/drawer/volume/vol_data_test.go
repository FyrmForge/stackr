package volume

import (
	"strings"
	"testing"
)

var dataView = View{Name: "uploads", Base: "/o/s/e/-/volumes/v1"}

func TestWipeForm(t *testing.T) {
	out := renderT(t, Settings(dataView, "", nil, Data{Wipe: true, Delete: true, Dest: "the local disk"}))
	wantAll(t, "wipe", out,
		"Wipe contents", `word="uploads"`, "/volumes/v1/wipe", `name="stop"`, "Stop tiles using it",
		`name="backup"`, "Back up first", "Backs up to the local disk first.", `include="input[type=checkbox]"`,
		"/volumes/v1/delete", "Deleting this volume removes it and its data at once.")
	if strings.Count(out, `name="backup"`) != 2 { // one per dialog
		t.Errorf("want a Back up first tick in the wipe and the delete\n%s", out)
	}
}

// The wipe and the delete dialog sit in one drawer: no id may repeat.
func TestDataSwitchIDsAreUnique(t *testing.T) {
	out := renderT(t, Settings(dataView, "", nil, Data{Wipe: true, Delete: true, Dest: "the local disk"}))
	seen := map[string]bool{}
	for _, part := range strings.Split(out, ` id="`)[1:] {
		id, _, _ := strings.Cut(part, `"`)
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if !seen["vol-wipe-uploads-backup-hint"] || !seen["vol-delete-uploads-backup-hint"] {
		t.Errorf("hint ids missing: %v", seen)
	}
}

func TestWipeFormWithoutDest(t *testing.T) {
	out := renderT(t, Settings(dataView, "", nil, Data{Wipe: true, Delete: true}))
	if strings.Contains(out, `name="backup"`) || !strings.Contains(out, `name="stop"`) {
		t.Errorf("no destination: no Back up first tick, the stop tick stays\n%s", out)
	}
}

// A removed volume offers Restore, never Wipe or Delete.
func TestRemovedSettingsOffersRestore(t *testing.T) {
	out := renderT(t, Settings(dataView, "", nil, Data{Restore: dataView.Base + "/restore-volume", Hint: "Add it back to the stack file to keep it."}))
	wantAll(t, "restore", out, "Restore", "/restore-volume", "Add it back to the stack file to keep it.")
	for _, no := range []string{"Delete volume", "Wipe contents"} {
		if strings.Contains(out, no) {
			t.Errorf("a removed volume offers %q", no)
		}
	}
}

func TestOverviewBackupAndRemoval(t *testing.T) {
	none := renderT(t, Overview(OverviewView{Docker: "d", Backup: &BackupFact{None: true, Words: "No backup", Setup: "/v?tab=backups"}}))
	wantAll(t, "no backup", none, "No backup", "Set up backup", `hx-get="/v?tab=backups"`)
	has := renderT(t, Overview(OverviewView{Docker: "d", Backup: &BackupFact{Words: "Every day at 03:00"}}))
	if strings.Contains(has, "Set up backup") || !strings.Contains(has, "Every day at 03:00") {
		t.Errorf("a scheduled volume\n%s", has)
	}
	gone := renderT(t, Overview(OverviewView{Docker: "d", Removal: &Removal{On: "02-01", Goes: "01-02", Restore: "/v/restore-volume", Hint: "Add it back to the stack file to keep it."}}))
	wantAll(t, "removed", gone, "Removed on 02-01. Its data is deleted on 01-02.", "Restore", "Add it back to the stack file to keep it.")
	if strings.Contains(gone, "Set up backup") {
		t.Error("a removed volume offers backup setup")
	}
}
