// Package storagetiles implements §2.7 storage tiles: server-scoped shares
// (nfs/smb) and local pools, consumed through declared sub-paths, each backed
// by a docker local-driver volume. stackr never mounts anything on the host,
// docker performs the mount at container start, so shares survive reboots
// with no fstab and no privileged helper.
package storagetiles

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Backends is the closed set.
var Backends = []string{"nfs", "smb", "local"}

func ValidBackend(b string) bool {
	return b == "nfs" || b == "smb" || b == "local"
}

// VolumeOpts builds the docker local-driver options for one sub-path.
func VolumeOpts(s *repo.Storage, p *repo.StoragePath) (map[string]string, error) {
	sub := strings.Trim(p.Subpath, "/")
	switch s.Backend {
	case "nfs":
		o := "addr=" + s.Address + ",rw,nfsvers=4"
		if s.Opts != "" {
			o = "addr=" + s.Address + "," + s.Opts
		}
		dev := ":" + path.Join("/", strings.TrimPrefix(s.Export, ":"), sub)
		return map[string]string{"type": "nfs", "o": o, "device": dev}, nil
	case "smb":
		o := "username=" + s.Username + ",password=" + s.Password + ",vers=3.0"
		if s.Opts != "" {
			o += "," + s.Opts
		}
		dev := "//" + s.Address + "/" + strings.Trim(s.Export, "/")
		if sub != "" {
			dev += "/" + sub
		}
		return map[string]string{"type": "cifs", "o": o, "device": dev}, nil
	case "local":
		if !strings.HasPrefix(s.Export, "/") {
			return nil, fmt.Errorf("local storage %s: export must be an absolute host path", s.Slug)
		}
		return map[string]string{"type": "none", "o": "bind", "device": path.Join(s.Export, sub)}, nil
	}
	return nil, fmt.Errorf("storage %s: unknown backend %q", s.Slug, s.Backend)
}

// EnsureVolume creates the sub-path's docker volume with the right driver
// opts if it doesn't exist yet. A volume that already exists is left alone,
// docker ignores differing opts on re-create, so edits are Recreate's job.
// The node comes from the storage's server, not from whoever is calling:
// a volume is a directory on one host, and creating an nfs mount on the
// manager when the operator picked a worker leaves the worker without the
// share it is about to mount, with swarm reporting the task healthy.
func EnsureVolume(ctx context.Context, c *cluster.Cluster, node string, s *repo.Storage, p *repo.StoragePath) (string, error) {
	name := repo.StorageVolume(p.ID)
	if c.VolumeExists(ctx, node, name) {
		return name, nil
	}
	opts, err := VolumeOpts(s, p)
	if err != nil {
		return "", err
	}
	return name, c.CreateVolumeOpts(ctx, node, name, "local", opts)
}

// Recreate drops and recreates the sub-path volume, the §2.7 edit dance
// (opts are immutable on a docker volume). Fails while consumers hold it.
func Recreate(ctx context.Context, c *cluster.Cluster, node string, s *repo.Storage, p *repo.StoragePath) error {
	name := repo.StorageVolume(p.ID)
	_ = c.RemoveVolume(ctx, node, name)
	opts, err := VolumeOpts(s, p)
	if err != nil {
		return err
	}
	return c.CreateVolumeOpts(ctx, node, name, "local", opts)
}

// Probe mounts the sub-path volume in a throwaway container and lists its
// root, surfacing bad creds / dead exports / missing local paths at
// create/edit time instead of at first deploy. Returns nil on success.
func Probe(ctx context.Context, c *cluster.Cluster, node string, s *repo.Storage, p *repo.StoragePath) error {
	name, err := EnsureVolume(ctx, c, node, s, p)
	if err != nil {
		return err
	}
	// No EnsureVolumeTool here: every volume op pulls the helper image itself
	// (runtime.volumeTool), and on a remote node this call would have pulled
	// it onto the manager instead.
	if _, err := c.ListVolumeFiles(ctx, node, name, "/"); err != nil {
		// Remove the broken volume so a fixed config recreates with new opts.
		_ = c.RemoveVolume(ctx, node, name)
		return err
	}
	return nil
}

// ValidateAttach enforces §2.7's state-placement rule: databases live on
// local-backed storage only, sqlite/postgres over NFS/SMB is a corruption
// class, so a network share on a managed instance is refused, not warned.
func ValidateAttach(s *repo.Storage, consumer *repo.Tile) error {
	if s.Backend != "local" && consumer.IsManaged() {
		return fmt.Errorf("%s is %s-backed; managed instances need local-backed storage (database files over %s corrupt)", s.Name, s.Backend, s.Backend)
	}
	return nil
}

// ensureForConsumer makes the sub-path's volume exist on whichever machine
// will actually mount it, which is not the same machine for the two kinds of
// backend:
//
//   - nfs and smb live on the network, so any node can mount them and a
//     replicated service can land on any of them. The volume is a definition,
//     not data, so it is made on every ready node and costs nothing.
//   - local is a directory on one host. Only that host can satisfy it, and
//     docker silently auto-creates an empty local volume for a name it does
//     not know, so a consumer that runs anywhere else gets an empty mount and
//     a healthy task rather than an error.
func ensureForConsumer(ctx context.Context, c *cluster.Cluster, store repo.Store, s *repo.Storage, p *repo.StoragePath, consumer *repo.Tile) (string, error) {
	home, err := c.NodeOfStorage(ctx, s)
	if err != nil {
		return "", err
	}
	if s.Backend != "local" {
		nodes, err := c.ListNodes(ctx)
		if err != nil {
			return "", err
		}
		// Best effort per node: a node whose agent is down cannot take the
		// definition, and failing the whole deploy for a machine the consumer
		// may never land on trades a working deploy for a tidy one. The last
		// error is only reported if no node took it.
		var name string
		var last error
		for _, n := range nodes {
			if n.Status() != "ready" {
				continue
			}
			got, err := EnsureVolume(ctx, c, n.ID, s, p)
			if err != nil {
				last = err
				continue
			}
			name = got
		}
		if name == "" {
			if last != nil {
				return "", fmt.Errorf("storage %s: no node could take it: %w", s.Slug, last)
			}
			return "", fmt.Errorf("storage %s: no ready node to mount it on", s.Slug)
		}
		return name, nil
	}
	// Two local pools on two machines cannot both be mounted by one task, and
	// whichever one loses gets an empty auto-created volume rather than an
	// error (docs/plans/39-codex-review-fixes.md, point 2).
	if nodes, _ := placement.StorageNodes(ctx, store, consumer); len(nodes) > 1 {
		return "", fmt.Errorf("%s attaches local pools on %d different machines; one task cannot mount both. Move the pools together, or use an nfs/smb share",
			consumer.Slug, len(nodes))
	}
	// A tile that resolves to a node has to resolve to this one. A tile that
	// resolves to none is unpinned, which on a single-node install is this
	// machine and on a multi-node one is the gap noted in docs/notes.md.
	if at, err := c.NodeOf(ctx, consumer); err == nil && at != home {
		return "", fmt.Errorf("storage %s is a local pool on another machine; %s runs elsewhere and would mount an empty directory. Move one of them, or use an nfs/smb share",
			s.Slug, consumer.Slug)
	}
	return EnsureVolume(ctx, c, home, s, p)
}

// ParseAttachment decodes one tiles.storage line. It lives in placement so
// that package can tell which pools a tile attaches without importing this
// one, which would close a cycle through cluster.
var ParseAttachment = placement.ParseAttachment

// Resolve turns one attachment line into a ready bind string, ensuring the
// backing volume exists and applying forced-ro.
func Resolve(ctx context.Context, c *cluster.Cluster, store repo.Store, consumer *repo.Tile, line string) (string, error) {
	slug, pathName, mount, ro, err := ParseAttachment(line)
	if err != nil {
		return "", err
	}
	s, err := store.GetStorageBySlug(ctx, slug)
	if err != nil || s == nil {
		return "", fmt.Errorf("storage %q: not found", slug)
	}
	if err := ValidateAttach(s, consumer); err != nil {
		return "", err
	}
	paths, err := store.ListStoragePaths(ctx, s.ID)
	if err != nil {
		return "", err
	}
	for i := range paths {
		if paths[i].Name != pathName {
			continue
		}
		name, err := ensureForConsumer(ctx, c, store, s, &paths[i], consumer)
		if err != nil {
			return "", err
		}
		bind := name + ":" + mount
		if ro || paths[i].ForcedRO {
			bind += ":ro"
		}
		return bind, nil
	}
	return "", fmt.Errorf("storage %s has no declared sub-path %q; declare it first (strict sub-paths)", slug, pathName)
}
