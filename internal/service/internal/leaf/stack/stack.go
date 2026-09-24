// Package stack owns stacks.
package stack

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct{ stacks store.StackStore }

func New(stacks store.StackStore) *Leaf { return &Leaf{stacks: stacks} }

func (l *Leaf) GetBySlug(ctx context.Context, orgID, slug string) (store.Stack, error) {
	return l.stacks.GetBySlug(ctx, orgID, slug)
}
