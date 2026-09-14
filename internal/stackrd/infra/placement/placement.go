// Package placement decides which node a tile's task may run on.
//
// Two rules, and the first one is the dangerous one:
//
//   - A pinned tile (it has a volume) runs on its home node and nowhere
//     else. A local volume is a directory on one host's disk, so a pinned
//     task that lands on a different node comes up against an empty volume
//     and the data is simply not there. Swarm reports that as a healthy
//     task. The home node is written on first deploy and changed only by a
//     completed move (docs/plans/30-docker-swarm.md, addendum).
//   - A tile with a node group runs only on nodes carrying that label.
//     Groups cascade org to stack to environment to tile like every other
//     setting (docs/plans/31-node-agent-open-questions.md, node groups).
package placement

import (
	"context"
	"fmt"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Plan is where one tile's task is allowed to run.
type Plan struct {
	// HomeNode is set for pinned tiles only.
	HomeNode string
	// Group is the resolved node group, "" = anywhere.
	Group string
	// Replicas is 1 for pinned tiles, the tile's own count otherwise.
	Replicas int
	Pinned   bool
}

// IsPinned reports whether a tile holds a volume, and so may never run more
// than one replica or move nodes on its own. The classifier is the existing
// one; no new field (docs/plans/30-docker-swarm.md, addendum).
// NodeOf is the node a tile's data is on: its home node, or for a volume tile
// the home node of the service that mounts it.
//
// A volume tile has no service and no placement of its own, the volume is a
// mount on the tile it is attached to, so that tile's node is where the
// directory actually is. Reading the volume tile's own column instead is how
// the file browser came to show the manager's copy of a volume that had been
// moved to a worker: no error, just the wrong files. Empty means "here".
func NodeOf(ctx context.Context, store repo.Store, t *repo.Tile) string {
	if t == nil {
		return ""
	}
	if t.IsVolume() && t.AttachedTileID != "" {
		if att, err := store.GetTile(ctx, t.AttachedTileID); err == nil && att != nil {
			if att.HomeNode != "" {
				return att.HomeNode
			}
			// The mounting tile may be placed by a local pool it attaches
			// rather than by a home node it has not been given yet. Answering
			// "" here would have the volume and the tile that mounts it
			// resolve to different machines.
			if nodes, _ := StorageNodes(ctx, store, att); len(nodes) == 1 {
				return nodes[0]
			}
			return ""
		}
	}
	if t.HomeNode == "" {
		// A tile attaching exactly one local pool belongs on that pool's node
		// even before a deploy has written it a home node. Answering "" here
		// is what let swarm schedule it anywhere and mount an empty directory.
		if nodes, _ := StorageNodes(ctx, store, t); len(nodes) == 1 {
			return nodes[0]
		}
	}
	return t.HomeNode
}

func IsPinned(ctx context.Context, store repo.Store, t *repo.Tile) bool {
	if t.IsVolume() || t.Volumes != "" || t.Engine != "" {
		return true
	}
	if _, local := StorageNodes(ctx, store, t); local {
		return true
	}
	siblings, err := store.ListTilesByEnv(ctx, t.EnvironmentID)
	if err != nil {
		// Unknown means pinned: a stateless tile pinned by mistake still
		// runs, a stateful one spread across nodes loses data.
		return true
	}
	for i := range siblings {
		if siblings[i].IsVolume() && siblings[i].AttachedTileID == t.ID {
			return true
		}
	}
	return false
}

// InGroup reports whether a pinned tile's data already sits on a node in the
// given group, so a config change asking for that group needs no volume move.
//
// One predicate, two callers: the plan view uses it to stop showing a move
// block the operator has already done, and the apply uses it to stop refusing
// on one. They have to agree, or the page offers a button the apply rejects.
func InGroup(ctx context.Context, store repo.Store, rt *runtime.Runtime, t *repo.Tile, group string) bool {
	if rt == nil || t.HomeNode == "" {
		// Cannot tell, and "cannot tell" must not clear a block. The home-node
		// check is also what keeps this read-only: For assigns and persists a
		// home node for a pinned tile that has none, and a predicate must not
		// write to the row it is asked about.
		return false
	}
	probe := *t
	probe.NodeGroup = group
	_, err := For(ctx, store, rt, &probe)
	return err == nil
}

// For resolves a tile's placement, assigning and persisting a home node the
// first time a pinned tile deploys.
//
// A pinned tile whose home node has left the swarm is refused rather than
// rescheduled: rescheduling it is exactly the silent data loss above. The
// operator's way out is Remove on the dead node, which says which volumes go
// with it.
func For(ctx context.Context, store repo.Store, rt *runtime.Runtime, t *repo.Tile) (Plan, error) {
	res := settings.ForTile(ctx, store, t)
	p := Plan{
		Group:    res.EffectiveGroup(t.NodeGroup),
		Replicas: max(t.Replicas, 1),
		Pinned:   IsPinned(ctx, store, t),
	}
	if !p.Pinned {
		return p, nil
	}
	p.Replicas = 1

	nodes, err := rt.ListNodes(ctx)
	if err != nil {
		return p, err
	}
	if t.HomeNode != "" {
		for _, n := range nodes {
			if n.ID != t.HomeNode {
				continue
			}
			if p.Group != "" && n.Group != p.Group {
				return p, fmt.Errorf("%s is pinned to node %s, which is not in group %q; move it first",
					t.Slug, n.Hostname, p.Group)
			}
			p.HomeNode = t.HomeNode
			return p, nil
		}
		return p, fmt.Errorf("%s is pinned to a node that has left the swarm; its volume is on that machine", t.Slug)
	}

	// First deploy: pick a node inside the group, preferring this one so a
	// single-node install never has to think about it.
	//
	// A tile attaching a local pool has no choice at all: the data is already
	// on that pool's machine, so preferring this one would pin it to the
	// manager and mount an empty directory there. The group still applies; a
	// pool outside the wanted group is a conflict the operator has to settle,
	// so say so rather than silently placing it elsewhere.
	if want, _ := StorageNodes(ctx, store, t); len(want) == 1 {
		for _, n := range nodes {
			if n.ID != want[0] {
				continue
			}
			if p.Group != "" && n.Group != p.Group {
				return p, fmt.Errorf("%s attaches a local pool on node %s, which is not in group %q; move the pool or drop the group",
					t.Slug, n.Hostname, p.Group)
			}
			if err := store.SetTileHomeNode(ctx, t.ID, n.ID); err != nil {
				return p, err
			}
			t.HomeNode, p.HomeNode = n.ID, n.ID
			return p, nil
		}
		return p, fmt.Errorf("%s attaches a local pool on a node that has left the swarm", t.Slug)
	}
	pick := ""
	for _, n := range nodes {
		if n.Status() != "ready" {
			continue
		}
		if p.Group != "" && n.Group != p.Group {
			continue
		}
		if n.Self {
			pick = n.ID
			break
		}
		if pick == "" {
			pick = n.ID
		}
	}
	if pick == "" {
		if p.Group != "" {
			return p, fmt.Errorf("no ready node in group %q for %s", p.Group, t.Slug)
		}
		return p, fmt.Errorf("no ready node for %s", t.Slug)
	}
	if err := store.SetTileHomeNode(ctx, t.ID, pick); err != nil {
		return p, err
	}
	t.HomeNode = pick
	p.HomeNode = pick
	return p, nil
}

// ParseAttachment decodes one tiles.storage line:
// "storage-slug/path-name:/mount[:ro]".
func ParseAttachment(line string) (storageSlug, pathName, mount string, ro bool, err error) {
	rest := line
	if strings.HasSuffix(rest, ":ro") {
		ro = true
		rest = strings.TrimSuffix(rest, ":ro")
	}
	i := strings.IndexByte(rest, ':')
	if i <= 0 || i == len(rest)-1 {
		return "", "", "", false, fmt.Errorf("storage %q: want storage/subpath:/mount[:ro]", line)
	}
	src, mnt := rest[:i], rest[i+1:]
	j := strings.IndexByte(src, '/')
	if j <= 0 || j == len(src)-1 {
		return "", "", "", false, fmt.Errorf("storage %q: source must be storage-slug/path-name", line)
	}
	if !strings.HasPrefix(mnt, "/") {
		return "", "", "", false, fmt.Errorf("storage %q: mount path must be absolute", line)
	}
	return src[:j], src[j+1:], mnt, ro, nil
}

// StorageNodes reports the swarm nodes the local-backed pools a tile attaches
// live on, deduped, and whether it attaches any local pool at all.
//
// A local pool is a directory on one host. Docker silently creates an empty
// local volume for a name it does not know, so a task scheduled anywhere else
// mounts nothing and reports healthy. That makes a storage attachment a pin
// just as much as a volume is, which IsPinned used to miss.
//
// A pool whose server has not joined the swarm yet contributes no node id: the
// tile is still pinned by the attachment, there is just no home to name yet.
func StorageNodes(ctx context.Context, store repo.Store, t *repo.Tile) (nodes []string, anyLocal bool) {
	if t == nil || t.Storage == "" {
		return nil, false
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(t.Storage, "\n") {
		line = strings.TrimSpace(line)
		// Same shape the deploy engine reads these with: blanks and comments
		// are not attachments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		slug, _, _, _, err := ParseAttachment(line)
		if err != nil {
			continue
		}
		s, err := store.GetStorageBySlug(ctx, slug)
		if err != nil || s == nil || s.Backend != "local" {
			continue
		}
		anyLocal = true
		sv, err := store.GetServer(ctx, s.ServerID)
		if err != nil || sv == nil || sv.NodeID == "" || seen[sv.NodeID] {
			continue
		}
		seen[sv.NodeID] = true
		nodes = append(nodes, sv.NodeID)
	}
	return nodes, anyLocal
}
