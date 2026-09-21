package managedtiles

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// SharedNet is the per-instance Docker network that consumers of a shared db
// join to reach it across environment/stack boundaries. Empty until the
// instance has claimed one; call ensureSharedNet first.
func SharedNet(instance *repo.Tile) string { return instance.SharedNet() }

// ensureSharedNet claims the instance's overlay out of the db pool the first
// time and records it on the row, so SharedNet answers for the rest of this
// call and for every later load. One overlay per provisioned instance is the
// point: consumers in dev must not be able to see consumers in prod
// (docs/plans/30-docker-swarm.md, decision 2).
func (s *Service) ensureSharedNet(ctx context.Context, instance *repo.Tile) error {
	_, err := netpool.ClaimDB(ctx, s.store, s.rows, s.c.Runtime(), instance)
	return err
}

// joinSharedNet claims the instance's overlay and puts it in the instance's
// service spec, waiting for the task that comes back.
//
// Connecting the running task's container instead would work until swarm
// replaced that task, and then every cross-env consumer would stop resolving
// the instance with nothing in the logs to say why: network attachments live
// in the spec, not on the container. The update rolls the task, so this is a
// short outage the first time an instance is provisioned from, and a no-op
// on every call after that, which is why the reconcile paths can call it
// freely.
func (s *Service) joinSharedNet(ctx context.Context, instance *repo.Tile) error {
	if err := s.ensureSharedNet(ctx, instance); err != nil {
		return err
	}
	name := s.ServiceName(ctx, instance)
	// Noted before the attach: WaitRolled needs a baseline, or a stale
	// "previous update completed" reads as this one having finished and the
	// caller execs into the container that is about to be killed.
	since := time.Now()
	rolled, err := s.c.AttachServiceNetwork(ctx, name,
		runtime.NetAttach{Name: SharedNet(instance), Aliases: instanceAliases(instance)})
	if err != nil {
		return err
	}
	if rolled {
		return s.c.WaitRolled(ctx, name, since, 90*time.Second)
	}
	return nil
}

// ScopeLabel is the human name for a tile's sharing scope.
func ScopeLabel(kind string) string {
	switch kind {
	case "stack":
		return "Whole stack"
	case "org":
		return "Whole organization"
	default:
		return "This environment"
	}
}

// orgOf returns the org id owning a tile's stack ("" on lookup failure).
func orgOf(ctx context.Context, store repo.Store, stackID string) string {
	if s, _ := store.GetStack(ctx, stackID); s != nil {
		return s.OrgID
	}
	return ""
}

// Eligible reports whether consumer may provision from instance given the
// instance's sharing scope: env = same environment, stack = same stack, org
// = same organization.
func Eligible(ctx context.Context, store repo.Store, instance, consumer *repo.Tile) bool {
	if !instance.IsManaged() || !CanProvision(instance.Engine) {
		return false
	}
	switch instance.ScopeKind {
	case "stack":
		return instance.StackID == consumer.StackID
	case "org":
		return orgOf(ctx, store, instance.StackID) == orgOf(ctx, store, consumer.StackID)
	default: // "env"
		return instance.EnvironmentID == consumer.EnvironmentID
	}
}

// ServesEnv reports whether an instance's sharing scope reaches into an
// environment, i.e. whether a slice cut for that env could ever be consumed
// from it.
//
// Worth checking at provision time and not only at attach: a slice records the
// env it belongs to, and AttachExisting refuses across envs, so a slice cut
// into the wrong env is a database nothing can ever use.
func ServesEnv(ctx context.Context, store repo.Store, instance *repo.Tile, env *repo.Environment) bool {
	// A synthetic consumer standing in that env, Eligible reads only these two
	// fields, and this keeps the scope rules in one place.
	return Eligible(ctx, store, instance, &repo.Tile{StackID: env.StackID, EnvironmentID: env.ID})
}

// EligibleInstances lists every provisionable shared db instance a consumer
// tile may use, across env/stack/org scope.
func EligibleInstances(ctx context.Context, store repo.Store, consumer *repo.Tile) ([]repo.Tile, error) {
	all, err := store.ListTiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []repo.Tile
	for i := range all {
		if all[i].ID == consumer.ID {
			continue
		}
		if Eligible(ctx, store, &all[i], consumer) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// instanceAliases are the DNS names the instance answers to on its shared
// network, the connection url uses the tile alias (rename-stable).
func instanceAliases(instance *repo.Tile) []string {
	return []string{instance.Slug, envnet.TileAlias(instance.ID)}
}
