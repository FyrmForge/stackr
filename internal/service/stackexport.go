package service

import (
	"context"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// ExportStack writes the stack live as a stackr-compose.yml
// (promote.Flow.Export): the whole ladder, or only env (a slug or id) when
// given.
func (o *Orchestrator) ExportStack(ctx context.Context, stackID, env string) ([]byte, error) {
	if _, err := o.stacks.Get(ctx, stackID); err != nil {
		return nil, err
	}
	var envID string
	if env != "" {
		e, err := o.envs.GetBySlug(ctx, stackID, env)
		if errors.Is(err, errs.ErrNotFound) {
			if e, err = o.envs.Get(ctx, env); err == nil && e.StackID != stackID {
				err = errs.ErrNotFound
			}
		}
		if err != nil {
			return nil, err
		}
		envID = e.ID
	}
	b, warns, err := o.promote.Export(ctx, stackID, envID)
	if err != nil {
		return nil, err
	}
	var head strings.Builder
	for _, w := range warns {
		head.WriteString("# warning: " + w + "\n")
	}
	return append([]byte(head.String()), b...), nil
}
