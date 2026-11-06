package service

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A slice dropped on_remove: drop is dumped first, from the instance's own
// env: the slice may sit in another one (shop:dev:pg from staging).
func TestDumpBeforeDropFindsTheInstanceInItsOwnEnv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	_, m, v := w.pg(t, "pg")
	w.fake.ExecOut = "SELECT 1;\n"
	p := store.Provision{ID: uuid.NewString(), InstanceID: m.ID, DBName: "shopdb", CreatedAt: time.Now()}

	other := uuid.NewString() // the slice's env, not the instance's
	must(t, w.orch.dumpBeforeDrop(ctx, other, p, io.Discard))
	runs, err := w.st.BackupRuns.ListByVolume(ctx, v.ID)
	must(t, err)
	if len(runs) != 1 {
		t.Fatalf("%d backups of the instance before the drop, want 1", len(runs))
	}

	p.InstanceID = "gone"
	if err := w.orch.dumpBeforeDrop(ctx, w.env, p, io.Discard); err == nil {
		t.Error("an instance with no volume was dropped from without a backup")
	}
}
