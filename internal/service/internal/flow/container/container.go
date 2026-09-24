// Package container is the tile-level container verbs: restart, stop and
// start every replica, each run as a job by the caller. Single-container
// verbs, logs and the terminal are leaf/tile's (one guard over all of them).
package container

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Flow struct{ Tiles *tile.Leaf }

// Restart restarts the tile's replicas one at a time, so a tile with more
// than one keeps serving.
func (f *Flow) Restart(ctx context.Context, t store.Tile, log io.Writer) error {
	return f.each(ctx, t, "restarting", func(id string) error { return f.Tiles.Restart(ctx, t.ID, id) }, log)
}

// Stop stops every replica; the tile shows stopped until started or deployed.
func (f *Flow) Stop(ctx context.Context, t store.Tile, log io.Writer) error {
	return f.each(ctx, t, "stopping", func(id string) error { return f.Tiles.Stop(ctx, t.ID, id) }, log)
}

func (f *Flow) Start(ctx context.Context, t store.Tile, log io.Writer) error {
	return f.each(ctx, t, "starting", func(id string) error { return f.Tiles.StartContainer(ctx, t.ID, id) }, log)
}

func (f *Flow) each(ctx context.Context, t store.Tile, verb string, do func(string) error, log io.Writer) error {
	cs, err := f.Tiles.Replicas(ctx, t)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return fmt.Errorf("%s has no containers; deploy it first", t.Slug)
	}
	var failed []error
	for _, c := range cs {
		_, _ = fmt.Fprintf(log, "%s %s\n", verb, c.Name)
		if err := do(c.ID); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", c.Name, err))
		}
	}
	return errors.Join(failed...)
}
