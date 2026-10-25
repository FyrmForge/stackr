package service

import (
	"context"
	"testing"
)

// The panel backup is off the box only once panel_backup_dest names a
// non-local destination.
func TestPanelBackupOffBox(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	check := func(want bool, when string) {
		t.Helper()
		got, err := w.orch.PanelBackupOffBox(ctx)
		if err != nil || got != want {
			t.Errorf("%s: off box = %v, %v; want %v", when, got, err, want)
		}
	}
	check(false, "unset")
	local, err := w.orch.localDest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", local.ID))
	check(false, "local destination")
	_, srv := newFakeS3(t)
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", w.globalS3(t, srv.URL).ID))
	check(true, "S3 destination")
}
