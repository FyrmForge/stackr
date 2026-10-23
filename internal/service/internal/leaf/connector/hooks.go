package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// SyncHooks points every connected git connection's provider deliveries at
// hook(id), the current panel URL's. A failing one is logged and skipped;
// the errors come back joined.
func (l *Leaf) SyncHooks(ctx context.Context, hook func(id string) string) error {
	cs, err := l.Gits(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cs {
		g, err := l.git(c)
		if err == nil {
			err = g.SetHook(ctx, hook(c.ID))
		}
		if err != nil {
			slog.Warn("connector webhook url sync", "connector", c.Name, "err", err)
			errs = append(errs, fmt.Errorf("connector %s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}
