// Package netpool hands out pre-created swarm overlays.
//
// A swarm service cannot join a network without rolling its tasks, so traefik
// cannot follow environments that come and go: it would roll on every new env
// and every PR preview. Instead the networks exist up front, traefik joins all
// of them once, and an environment takes a free one and gives it back
// (docs/plans/30-docker-swarm.md, decision 2).
//
// The consequence is that a network name means nothing on its own. The only
// mapping from network to owner is the DB column, environments.network for an
// env, tiles.shared_net for a shared managed instance. Nothing parses a name.
package netpool

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const (
	// EnvPrefix names the environment pool, DBPrefix the pool a shared
	// managed db draws from so consumers in dev cannot see consumers in prod.
	EnvPrefix = "stkr-net-"
	DBPrefix  = "stkr-dbnet-"

	// Sizes pre-created at boot. Not a ceiling: a claim past the end creates
	// the next name, which is all growth means while traefik is still a
	// container that can hot-join. Once traefik is a service (step 4) growth
	// has to roll it, and that is where the warning on the plan page lands.
	EnvPoolSize = 32
	DBPoolSize  = 8
)

// EnvOwner and TileOwner are the two rows a claim writes: an environment's
// network column, and a shared managed instance's.
//
// They are interfaces declared here rather than the services themselves
// because the dependency runs the other way — the service package is built on
// top of this one, so this one cannot name it. EnvironmentService and
// TileService are what satisfy them.
//
// Two interfaces rather than one, so each caller passes the single service it
// already holds instead of an adapter that exists only to carry the other.
//
// The pool decides which name is free. Who holds it is not the pool's to
// write, and it used to write it anyway.
type EnvOwner interface {
	SetNetwork(ctx context.Context, envID, network string) error
}

type TileOwner interface {
	SetSharedNet(ctx context.Context, tileID, name string) error
}

// One panel process owns the pool, so a mutex is the whole allocator: claim
// reads the used set and writes the winner under it.
// a DB unique index on the column is the upgrade path if a second
// writer ever exists.
var mu sync.Mutex

func name(prefix string, n int) string { return fmt.Sprintf("%s%02d", prefix, n) }

// Ensure pre-creates both pools. Called at boot, before anything claims.
func Ensure(ctx context.Context, rt *runtime.Runtime) error {
	for _, p := range []struct {
		prefix string
		n      int
	}{{EnvPrefix, EnvPoolSize}, {DBPrefix, DBPoolSize}} {
		for i := 1; i <= p.n; i++ {
			if err := rt.EnsureOverlay(ctx, name(p.prefix, i)); err != nil {
				return fmt.Errorf("create %s: %w", name(p.prefix, i), err)
			}
		}
	}
	return nil
}

// claim returns the first name in the pool that no row holds, creating the
// network if the pool has to grow past its pre-created size.
func claim(ctx context.Context, store repo.Store, rt *runtime.Runtime, prefix string) (string, error) {
	held, err := store.ClaimedNetworks(ctx)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, h := range held {
		used[h] = true
	}
	n := firstFree(used, prefix)
	if err := rt.EnsureOverlay(ctx, n); err != nil {
		return "", err
	}
	if prefix == EnvPrefix {
		// Traefik holds the whole pool from the moment it starts, so this is
		// a no-op for the 32 pre-created overlays. It only bites when the pool
		// has grown past them: an environment on a network traefik's spec does
		// not carry is simply unrouted, with nothing in any log to say so.
		// The cost when it does fire is one stop-first roll, a short gap on
		// 80 and 443 (docs/plans/30-docker-swarm.md, decision 2).
		if _, err := rt.AttachServiceNetworks(ctx, runtime.TraefikService,
			[]runtime.NetAttach{{Name: n}}); err != nil {
			return "", err
		}
	}
	return n, nil
}

// firstFree is the allocator: the lowest-numbered name in the pool that no row
// holds. It walks past the pre-created size rather than failing; that is what
// growing the pool means, and claim() rolls traefik onto the new overlay.
func firstFree(used map[string]bool, prefix string) string {
	for i := 1; ; i++ {
		if n := name(prefix, i); !used[n] {
			return n
		}
	}
}

// ClaimEnv returns the environment's overlay, taking a free one from the pool
// the first time. Idempotent: an env that already holds one keeps it.
func ClaimEnv(ctx context.Context, store repo.Store, own EnvOwner, rt *runtime.Runtime, envID string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	env, err := store.GetEnvironment(ctx, envID)
	if err != nil {
		return "", err
	}
	if env == nil {
		return "", fmt.Errorf("environment %s: gone", envID)
	}
	if env.Network != "" {
		// The row is the truth, but the network itself can be missing after a
		// docker prune; recreating it under the same name keeps the row valid.
		return env.Network, rt.EnsureOverlay(ctx, env.Network)
	}
	n, err := claim(ctx, store, rt, EnvPrefix)
	if err != nil {
		return "", err
	}
	return n, own.SetNetwork(ctx, envID, n)
}

// ClaimDB is ClaimEnv for a shared managed instance's own network.
func ClaimDB(ctx context.Context, store repo.Store, own TileOwner, rt *runtime.Runtime, t *repo.Tile) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if t.SharedNetName != "" {
		return t.SharedNetName, rt.EnsureOverlay(ctx, t.SharedNetName)
	}
	n, err := claim(ctx, store, rt, DBPrefix)
	if err != nil {
		return "", err
	}
	if err := own.SetSharedNet(ctx, t.ID, n); err != nil {
		return "", err
	}
	t.SharedNetName = n
	return n, nil
}

// drain empties a pooled network of everything that is not infrastructure,
// before the name goes back into circulation. Anything left behind would be
// inherited by whichever environment claims the name next, and that is the one
// failure this pool must never have.
//
// Two kinds of tenant and two different handles. A plain container is
// disconnected. A swarm task cannot be: its attachment is in the service spec,
// so a disconnect is undone within seconds, and the service is what has to
// leave. DisconnectTenants skips swarm tasks for exactly that reason, which
// made this whole drain a no-op once every tile became a service.
func drain(ctx context.Context, rt *runtime.Runtime, name string) error {
	if _, err := rt.DisconnectTenants(ctx, name); err != nil {
		return err
	}
	svcs, err := rt.NetworkServices(ctx, name)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if _, err := rt.DetachServiceNetwork(ctx, svc, name); err != nil {
			return fmt.Errorf("detaching %s from %s: %w", svc, name, err)
		}
	}
	return nil
}

// ReleaseEnv gives an environment's network back. The network object itself
// stays, it is pooled, not per-tenant, and traefik is on it.
func ReleaseEnv(ctx context.Context, own EnvOwner, rt *runtime.Runtime, env *repo.Environment) error {
	if env == nil || env.Network == "" {
		return nil
	}
	if err := drain(ctx, rt, env.Network); err != nil {
		return err
	}
	return own.SetNetwork(ctx, env.ID, "")
}

// ReleaseDB is ReleaseEnv for a shared managed instance.
func ReleaseDB(ctx context.Context, own TileOwner, rt *runtime.Runtime, t *repo.Tile) error {
	if t == nil || t.SharedNetName == "" {
		return nil
	}
	if err := drain(ctx, rt, t.SharedNetName); err != nil {
		return err
	}
	t.SharedNetName = ""
	return own.SetSharedNet(ctx, t.ID, "")
}

// Sweep clears out pooled networks that no row holds any more. Shaped like
// runtime.SweepForwardState: a crash between deleting the owner row and
// disconnecting its containers leaves a network that is free by the DB and
// occupied in reality, so the next environment to claim it starts with a
// neighbour it can reach. Best-effort, boot only.
// Backfill gives an overlay to every environment that has none.
//
// Migration 009 added environments.network with an empty default and
// backfilled nothing, and the only thing that ever fills it is ClaimEnv, which
// runs at deploy. So on an upgraded install every environment that already
// existed sat on no overlay until somebody happened to redeploy it: port
// forward refused with "tile is not on a network yet", and the proxy left the
// whole environment out of its network-to-env map without saying anything.
//
// Runs at boot next to Sweep. A pool with nothing left is logged and skipped
// rather than fatal, an install that cannot claim is still an install that
// should come up and say so.
func Backfill(ctx context.Context, store repo.Store, own EnvOwner, rt *runtime.Runtime) {
	ids, err := store.EnvironmentsWithoutNetwork(ctx)
	if err != nil {
		slog.Error("overlay backfill: cannot list environments", "error", err)
		return
	}
	for _, id := range ids {
		net, err := ClaimEnv(ctx, store, own, rt, id)
		if err != nil {
			slog.Error("overlay backfill: no network for environment, its tiles stay off the overlay "+
				"until it is deployed", "environment", id, "error", err)
			continue
		}
		slog.Info("overlay backfill: environment claimed a network", "environment", id, "network", net)
	}
}

func Sweep(ctx context.Context, store repo.Store, rt *runtime.Runtime) {
	held, err := store.ClaimedNetworks(ctx)
	if err != nil {
		return
	}
	claimed := map[string]bool{}
	for _, h := range held {
		claimed[h] = true
	}
	nets, err := rt.ListNetworks(ctx, "")
	if err != nil {
		return
	}
	for _, n := range nets {
		if claimed[n] || !pooled(n) {
			continue
		}
		_, _ = rt.DisconnectTenants(ctx, n)
		// A stray task cannot be disconnected, its service would put it
		// straight back. The service itself is what has to go, and on an
		// unclaimed pool network there is no row left that wants it.
		svcs, err := rt.NetworkServices(ctx, n)
		if err != nil {
			continue
		}
		for _, svc := range svcs {
			slog.Warn("netpool sweep: removing orphan service", "service", svc, "network", n)
			_ = rt.RemoveService(ctx, svc)
		}
	}
}

func pooled(n string) bool {
	return strings.HasPrefix(n, EnvPrefix) || strings.HasPrefix(n, DBPrefix)
}
