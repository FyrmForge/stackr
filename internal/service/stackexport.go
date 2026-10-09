package service

import (
	"context"
	"errors"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// ExportStack writes one env of the stack live as a stackr-compose.yml
// (promote.Flow.Export). env is a slug or id; "" is the first rung of the
// ladder.
func (o *Orchestrator) ExportStack(ctx context.Context, stackID, env string) ([]byte, error) {
	if _, err := o.stacks.Get(ctx, stackID); err != nil {
		return nil, err
	}
	var e Environment
	if env == "" {
		ladder, err := o.envs.Ladder(ctx, stackID)
		if err != nil {
			return nil, err
		}
		if len(ladder) == 0 {
			return nil, errs.Conflictf("This stack has no environment to export.")
		}
		e = ladder[0]
	} else {
		var err error
		if e, err = o.envs.GetBySlug(ctx, stackID, env); errors.Is(err, errs.ErrNotFound) {
			if e, err = o.envs.Get(ctx, env); err == nil && e.StackID != stackID {
				err = errs.ErrNotFound
			}
		}
		if err != nil {
			return nil, err
		}
	}
	b, warns, err := o.promote.Export(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	var head strings.Builder
	for _, w := range warns {
		head.WriteString("# warning: " + w + "\n")
	}
	return append([]byte(head.String()), b...), nil
}
