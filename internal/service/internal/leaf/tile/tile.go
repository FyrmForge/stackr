// Package tile owns tiles.
package tile

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct{ tiles store.TileStore }

func New(tiles store.TileStore) *Leaf { return &Leaf{tiles: tiles} }

func (l *Leaf) GetBySlug(ctx context.Context, envID, slug string) (store.Tile, error) {
	return l.tiles.GetBySlug(ctx, envID, slug)
}
