package service

import (
	"context"
	"testing"
)

// An env's volumes are orphaned with it (the environments_scope_cascade
// trigger), so the orphan job still backs them up.
func TestDeleteEnvOrphansItsVolumes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	v, _, err := w.orch.DeclareEnvVolume(ctx, w.env, "data")
	must(t, err)
	must(t, w.st.Environments.Delete(ctx, w.env))
	got, err := w.orch.Volume(ctx, v.ID)
	if err != nil || got.OrphanedAt == nil {
		t.Fatalf("volume after env delete = %+v, %v; want an orphan", got, err)
	}
}
