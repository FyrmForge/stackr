package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
)

// A rollback to an imported (outside) image keeps the org's import copy in the
// registry sweep, as the current-tile path does.
func TestRegistryCleanKeepsImportedRollbackImage(t *testing.T) {
	t.Parallel()
	r := newRecipeRig(t)
	ctx := context.Background()
	tl, _, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.setPGUp(ctx, r.inst.ID, pgUp{FromImage: "postgres:16.4", ToImage: image.ClusterRegistry + "o/s/pg:to"}))
	refs := map[string]bool{}
	r.orch.keepOperatedRefs(ctx, tl, "o", refs)
	repo, tag := regfront.ImportName("o", "postgres:16.4")
	if !refs[repo+":"+tag] || !refs["o/s/pg:to"] {
		t.Errorf("refs = %v: the imported rollback image is swept", refs)
	}
}
