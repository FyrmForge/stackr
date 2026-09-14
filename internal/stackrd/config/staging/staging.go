// Package staging records pending UI edits into the staged_changes buffer so
// they apply as one reviewed transaction (see internal/stackconf/staging.go
// for how the buffer is drained). It is the shared write-side used by every
// structural UI handler on a UI-managed stack.
package staging

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Ops for a staged change.
const (
	OpUpdate = "update" // sparse patch onto the committed tile
	OpCreate = "create" // patch is a full TileConf, added as a new tile
	OpDelete = "delete" // tile dropped so apply emits a strict-mode delete
)

// Stage records (or replaces) a pending change for a tile's field-group.
// One row per (env, tile_slug, summary): re-staging the same group replaces
// its prior row, so per-group last-write-wins. patch is the sparse (update)
// or full (create) config; nil for delete.
func Stage(ctx context.Context, store repo.Store, tile *repo.Tile, authorID, authorName, summary, op string, patch any) error {
	if existing, err := store.ListStagedByEnv(ctx, tile.EnvironmentID); err == nil {
		for i := range existing {
			if existing[i].TileSlug == tile.Slug && existing[i].Summary == summary {
				if err := store.DeleteStagedChange(ctx, existing[i].ID); err != nil {
					slog.Error("superseded staged change not deleted", "change", existing[i].ID, "tile", tile.Slug, "error", err)
				}
			}
		}
	}
	body := map[string]any{"op": op}
	if patch != nil {
		pj, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		body["patch"] = json.RawMessage(pj)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return store.CreateStagedChange(ctx, &repo.StagedChange{
		ID:         uuid.NewString(),
		StackID:    tile.StackID,
		EnvID:      tile.EnvironmentID,
		TileSlug:   tile.Slug,
		AuthorID:   authorID,
		AuthorName: authorName,
		Summary:    summary,
		Payload:    string(payload),
		CreatedAt:  time.Now().UTC(),
	})
}
