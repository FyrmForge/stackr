// Package envnet maps a tile to its environment's isolated Docker network
// and slug-based naming. Each environment holds one attachable overlay out of
// a pool (infra/netpool), the name says nothing, the env row is the mapping.
// Traefik is attached to every one so it can route HTTP to any tile, while
// tiles themselves can only reach their own environment (and a shared
// instance over its own network, repo.Tile.SharedNet).
//
// Every docker object name is built here and nowhere else. Names are made of
// slugs joined by "_": Slugify emits only [a-z0-9-], so no slug can contain
// an underscore and the segments cannot be confused. A "-" separator would be
// ambiguous (stack "a-b" + env "c" reads the same as "a" + "b-c"), which is
// the collision this scheme exists to remove. Names are never parsed back;
// anything that needs the rows goes to the rows.
//
// Nothing resolves a tile by container name, tiles get explicit DNS aliases
// (the tile slug and TileAlias), so the separator is operator-facing only.
package envnet

import (
	"context"
	"fmt"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Prefix opens every docker object stackr owns: networks, containers and the
// panel's own traefik/registry/relay containers. Images use ImagePrefix.
// Volumes are the exception, they keep the older "stackr-" prefix, because
// docker cannot rename a volume and a prefix change would detach live data
// (repo.StorageVolume).
const (
	Prefix      = "stkr_"
	ImagePrefix = "stkr/"
	sep         = "_"

	// maxObjectName is swarm's limit on a service name. Docker returns
	// "name must be 63 characters or fewer" and nothing else.
	maxObjectName = 63
)

// Scope is the resolved naming context for one tile.
type Scope struct {
	OrgSlug   string
	StackSlug string
	EnvSlug   string
}

// namePrefix is the operator-facing naming root for one environment's docker
// objects: stkr_<org>_<stack>_<env>, or stkr_<org>_<stack> for the stack's
// home (shared tiles, repo.HomeSlug). It is a name, not a network, since the
// swarm move the network an environment actually sits on comes out of a pool
// (infra/netpool) and is stored on the env row, so the two must not be the
// same function. Making this one a lookup would churn every container name.
func (s Scope) namePrefix() string {
	if s.EnvSlug == repo.HomeSlug {
		return Prefix + s.OrgSlug + sep + s.StackSlug
	}
	return Prefix + s.OrgSlug + sep + s.StackSlug + sep + s.EnvSlug
}

// ContainerName is the operator-facing container name for a tile; suffix
// distinguishes coexisting containers (e.g. deployment id) and may be empty.
// The suffix joins on the same separator: a slug can never contain one, so a
// tile named "site" with suffix "abc" cannot collide with a tile named
// "site_abc", no such slug exists.
func (s Scope) ContainerName(tileSlug, suffix string) string {
	name := s.namePrefix() + sep + tileSlug
	if suffix == "" {
		return name
	}
	// Swarm refuses a service name over 63 characters, and a one-shot run's
	// name is the tile's plus a suffix, so a tile whose own name fits could
	// still push its runs over: every cron run of a tile four scopes deep
	// failed with "name must be 63 characters or fewer" and the panel showed
	// only the daemon's words.
	//
	// The suffix is what makes the name unique, so the tile slug gives way,
	// not the run id. Names are operator-facing only and nothing parses them
	// back, so a shortened one costs nothing but readability.
	if over := len(name) + len(sep) + len(suffix) - maxObjectName; over > 0 {
		if keep := len(tileSlug) - over; keep > 0 {
			name = s.namePrefix() + sep + tileSlug[:keep]
		} else {
			name = s.namePrefix()
		}
	}
	return name + sep + suffix
}

// ServiceName is the swarm service for a tile: the scoped name with no
// suffix. Stable for the life of the tile, a deploy updates the service in
// place and swarm rolls the tasks, so unlike the old per-deployment container
// name there is nothing to disambiguate.
func (s Scope) ServiceName(tileSlug string) string { return s.ContainerName(tileSlug, "") }

// ImageRepo is the local image repository for a tile's builds. No env in it:
// an image is a property of (stack, tile, commit), and baking the env in would
// make promoting a built image between environments impossible.
func (s Scope) ImageRepo(tileSlug string) string {
	return ImagePrefix + s.OrgSlug + sep + s.StackSlug + sep + tileSlug
}

// UpperEnv reports whether the tile lives above the default env, the first
// static env of its stack. Upper rungs get images by Promote from a plan row,
// so a build from branch head is refused there. store order, like
// the pr hook and allAuto.
func UpperEnv(ctx context.Context, store repo.Store, t *repo.Tile) bool {
	envs, err := store.ListEnvironmentsByStack(ctx, t.StackID)
	if err != nil {
		return false
	}
	for i := range envs {
		if envs[i].Type == "static" {
			return envs[i].ID != t.EnvironmentID
		}
	}
	return false
}

// TileAlias is the tile's globally-unique DNS alias, used by Traefik which
// sits on every environment network (plain slugs would collide across envs).
func TileAlias(tileID string) string { return "tile-" + tileID[:8] }

// Resolve looks up the tile's stack and environment slugs.
func Resolve(ctx context.Context, store repo.Store, t *repo.Tile) (Scope, error) {
	env, err := store.GetEnvironment(ctx, t.EnvironmentID)
	if err != nil {
		return Scope{}, err
	}
	st, err := store.GetStack(ctx, t.StackID)
	if err != nil {
		return Scope{}, err
	}
	if env == nil || st == nil {
		return Scope{}, fmt.Errorf("tile %s: missing stack or environment", t.ID)
	}
	return ForStack(ctx, store, st, env.Slug)
}

// ForStack builds the scope for one environment of a stack, loading the org
// for its slug. Callers that hold the rows already use this over Resolve.
func ForStack(ctx context.Context, store repo.Store, st *repo.Stack, envSlug string) (Scope, error) {
	org, err := store.GetOrg(ctx, st.OrgID)
	if err != nil {
		return Scope{}, err
	}
	if org == nil {
		return Scope{}, fmt.Errorf("stack %s: missing org", st.ID)
	}
	return Scope{OrgSlug: org.Slug, StackSlug: st.Slug, EnvSlug: envSlug}, nil
}

// Ensure resolves the tile's scope and claims the environment's overlay out of
// the pool if it has none yet. Returns the scope and the network name; nothing
// may derive the network from the scope.
//
// Traefik is deliberately not attached here. It is a service now, and a
// service joining a network rolls every one of its tasks, an environment
// create would take 80 and 443 down. Traefik holds the whole pool from the
// moment it starts (infra/proxy), so the network this claims is already one
// of its own.
func Ensure(ctx context.Context, store repo.Store, c *cluster.Cluster, t *repo.Tile) (Scope, string, error) {
	sc, err := Resolve(ctx, store, t)
	if err != nil {
		return Scope{}, "", err
	}
	// netpool shapes overlays, which is swarm state and the manager's own.
	netName, err := netpool.ClaimEnv(ctx, store, c.Runtime(), t.EnvironmentID)
	if err != nil {
		return Scope{}, "", err
	}
	// Recording here is what lets a tile in a just-created env resolve
	// ${{ stackr.PROXY_CIDR }}: the deploy engine calls Ensure before it
	// resolves variables, so the value is in place by the time it is read.
	RecordProxyAddr(ctx, store, c, t.EnvironmentID, netName)
	return sc, netName, nil
}

// ServiceFor resolves the swarm service name for a tile ("" if its stack or
// environment is already gone, which teardown treats as nothing to remove).
func ServiceFor(ctx context.Context, store repo.Store, t *repo.Tile) string {
	sc, err := Resolve(ctx, store, t)
	if err != nil {
		return ""
	}
	return sc.ServiceName(t.Slug)
}

// TearDown removes everything docker holds for one tile: its service, and any
// container left under the tile's labels. Both halves are needed, removing a
// task's container only makes swarm start another, and a box that deployed
// before the swarm move still has plain containers under the same labels.
//
// Best-effort throughout: a tile whose objects are already gone must not
// strand its rows.
func TearDown(ctx context.Context, store repo.Store, c *cluster.Cluster, t *repo.Tile) {
	if c == nil {
		return
	}
	if name := ServiceFor(ctx, store, t); name != "" {
		_ = c.RemoveService(ctx, name)
	}
	// The label listing is the manager's own containers, which is all a
	// pre-swarm leftover can be: everything since the swarm move is a task,
	// and a task's container goes when its service does, above.
	for _, label := range []string{runtime.LabelApp, runtime.LabelDB} {
		cs, err := c.ListByLabel(ctx, label, t.ID)
		if err != nil {
			continue
		}
		for _, ct := range cs {
			_ = c.StopRemove(ctx, c.Self(ctx), ct.ID)
		}
	}
}

// Net is the overlay an environment sits on, without claiming one. Empty
// means nothing has deployed into the env yet.
func Net(ctx context.Context, store repo.Store, envID string) (string, error) {
	env, err := store.GetEnvironment(ctx, envID)
	if err != nil || env == nil {
		return "", err
	}
	return env.Network, nil
}

// RecordProxyAddr stores where Traefik sits on one environment network, feeding
// ${{ stackr.PROXY_IP }} / ${{ stackr.PROXY_CIDR }}. Call it after attaching.
//
// Best-effort throughout: this is a convenience for config files, and no
// failure here should stop a deploy or stop Traefik serving. A missed write
// leaves the previous value standing until the next attach.
func RecordProxyAddr(ctx context.Context, store repo.Store, c *cluster.Cluster, envID, netName string) {
	if envID == "" || c == nil {
		return
	}
	cs, err := c.ListByLabel(ctx, "stackr.traefik", "true")
	if err != nil || len(cs) == 0 {
		return
	}
	ip, cidr, err := c.NetworkMemberAddr(ctx, netName, cs[0].ID)
	if err != nil || ip == "" {
		return
	}
	_ = store.SetEnvironmentProxy(ctx, envID, ip, cidr)
}
