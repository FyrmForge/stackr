package graph

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// EnvWord is an env's worst-of tile word (the stack canvas's env card
// status): "" when the env has no tiles.
func (f *Flow) EnvWord(ctx context.Context, envID string) (string, error) {
	ts, err := f.Tiles.List(ctx, envID)
	if err != nil {
		return "", err
	}
	return f.worstOf(ctx, "", ts)
}

// TileWord is the word the tile's card reads.
func (f *Flow) TileWord(ctx context.Context, t store.Tile) (string, error) {
	s, err := f.status(ctx, t)
	return s.word, err
}
