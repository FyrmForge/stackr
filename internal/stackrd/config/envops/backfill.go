package envops

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// BackfillVariables projects existing tiles' env blobs into `variables` rows.
//
// The resolver reads rows only. Rows are written when a tile is written, so an
// upgraded install has tiles whose env exists solely in the blob, and those
// apps would deploy with an empty environment, silently, until someone happened
// to re-save them. This runs once at startup after the cipher is loaded (the
// migration itself can't do it: the blob is encrypted).
//
// Skips any tile that already has rows, so a structured edit is never
// overwritten by a stale blob.
//
// goes away with the blob at R7's follow-up, along with projectEnvVars.
func BackfillVariables(ctx context.Context, store repo.Store) error {
	tiles, err := store.ListTiles(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range tiles {
		t := &tiles[i]
		if t.Env == "" {
			continue
		}
		existing, err := store.ListVariables(ctx, repo.OwnerTile, t.ID)
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			continue
		}
		for _, v := range envutil.Parse(t.Env) {
			if v.Key == "" {
				continue
			}
			if err := store.UpsertVariable(ctx, &repo.Variable{OwnerKind: repo.OwnerTile,
				OwnerID: t.ID, Name: v.Key, Value: v.Value, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
		}
	}
	return nil
}
