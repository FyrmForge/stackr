package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// SyncHooks points every connected connector's GitHub App webhook at the
// current panel URL. A failing App is logged and skipped; the errors come
// back joined.
func (l *Leaf) SyncHooks(ctx context.Context) error {
	cs, err := l.conns.ListAll(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cs {
		app := parse(c).App
		if app == nil {
			continue
		}
		if err := l.app.SetHookURL(ctx, c.ID, *app); err != nil {
			slog.Warn("github webhook url sync", "connector", c.Name, "err", err)
			errs = append(errs, fmt.Errorf("connector %s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}
