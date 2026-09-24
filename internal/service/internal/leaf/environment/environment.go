// Package environment owns environments.
package environment

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type Leaf struct{ envs store.EnvironmentStore }

func New(envs store.EnvironmentStore) *Leaf { return &Leaf{envs: envs} }

func (l *Leaf) GetBySlug(ctx context.Context, stackID, slug string) (store.Environment, error) {
	return l.envs.GetBySlug(ctx, stackID, slug)
}
