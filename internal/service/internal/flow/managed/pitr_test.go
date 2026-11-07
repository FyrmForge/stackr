package managed_test

import (
	"slices"
	"strings"
	"testing"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// PITR on: the archive volume and the archiving Command join the container,
// "base" joins the methods and has an argv; off, none of them.
func TestContainerPITR(t *testing.T) {
	w := setup(t, "postgres")
	off, err := w.f.Container(ctx, w.db)
	must(t, err)
	if len(off.Cmd) != 0 {
		t.Errorf("PITR off: cmd = %v", off.Cmd)
	}
	if ms, _ := w.f.Methods(ctx, w.db); !slices.Equal(ms, []string{"dump"}) {
		t.Errorf("PITR off: methods = %v", ms)
	}
	if argv, _ := w.f.Backup(ctx, w.db, "base"); argv != nil {
		t.Errorf("PITR off: base argv = %v", argv)
	}
	_, err = w.f.Instances.SetPITR(ctx, w.instance, 7)
	must(t, err)
	on, err := w.f.Container(ctx, w.db)
	must(t, err)
	if len(on.Binds) != 2 || !strings.HasSuffix(on.Binds[1], ":"+mflow.PGArchiveDir) {
		t.Fatalf("binds = %v", on.Binds)
	}
	cmd := strings.Join(on.Cmd, " ")
	for _, want := range []string{
		"archive_mode=on",
		"archive_timeout=60",
		"archive_command=test ! -f " + mflow.PGArchiveDir + "/%f && cp %p " + mflow.PGArchiveDir + "/.%f && mv",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("cmd lacks %q: %s", want, cmd)
		}
	}
	if ms, _ := w.f.Methods(ctx, w.db); !slices.Equal(ms, []string{"dump", "base"}) {
		t.Errorf("PITR on: methods = %v", ms)
	}
	argv, _ := w.f.Backup(ctx, w.db, "base")
	if got := strings.Join(argv, " "); !strings.Contains(got, "pg_basebackup -U stackr -D - -Ft -Xnone -z") {
		t.Errorf("base argv = %v", argv)
	}
	ws, _ := w.f.WalStart(ctx, w.db)
	if got := strings.Join(ws, " "); !strings.Contains(got, "pg_walfile_name(pg_current_wal_lsn())") {
		t.Errorf("wal start argv = %v", ws)
	}
	vs, _ := w.f.Volumes.List(ctx, volume.Scope{Kind: "env", ID: w.db.EnvironmentID})
	var arch int
	for _, v := range vs {
		if mflow.IsArchiveVolume(v) {
			arch++
		}
	}
	if len(vs) != 2 || arch != 1 {
		t.Errorf("volumes = %d, archive volumes = %d", len(vs), arch)
	}
}
