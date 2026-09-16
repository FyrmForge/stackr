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

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
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
func VolumeOpts(s *repo.Storage, subpath string) (map[string]string, error) {
	sub := strings.Trim(subpath, "/")
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
//
// name is repo.StorageVolume for a declared path, repo.OrgShareVolume for an
// org share's sub-path.
func EnsureVolume(ctx context.Context, c *cluster.Cluster, node, name string, s *repo.Storage, subpath string) (string, error) {
	if c.VolumeExists(ctx, node, name) {
		return name, nil
	}
	opts, err := VolumeOpts(s, subpath)
	if err != nil {
		return "", err
	}
	return name, c.CreateVolumeOpts(ctx, node, name, "local", opts)
}

// DropOrgShareVolumes removes every sub-path volume of an org share on every
// ready node, the org share's edit dance: opts are immutable on a docker
// volume, and the next deploy of each consumer recreates its volume with the
// new ones. Docker refuses to remove a volume a container still mounts, so
// this fails while consumers run, and the caller must not save the edit.
func DropOrgShareVolumes(ctx context.Context, c *cluster.Cluster, s *repo.Storage) error {
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return err
	}
	prefix := "stackr-stor-" + s.ID[:8] + "-"
	for _, n := range nodes {
		if n.Status() != "ready" {
			continue
		}
		vols, err := c.ListVolumes(ctx, n.ID)
		if err != nil {
			return err
		}
		for _, v := range vols {
			if !strings.HasPrefix(v.Name, prefix) {
				continue
			}
			if err := c.RemoveVolume(ctx, n.ID, v.Name); err != nil {
				return fmt.Errorf("share %s is still mounted on %s; stop its tiles first: %w", s.Slug, n.Hostname, err)
			}
		}
	}
	return nil
}

// Probe mounts the sub-path volume in a throwaway container and lists its
// root, surfacing bad creds / dead exports / missing local paths at
// create/edit time instead of at first deploy. Returns nil on success.
func Probe(ctx context.Context, c *cluster.Cluster, node string, s *repo.Storage, p *repo.StoragePath) error {
	name, err := EnsureVolume(ctx, c, node, repo.StorageVolume(p.ID), s, p.Subpath)
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
		return ensureEverywhere(ctx, c, repo.StorageVolume(p.ID), s, p.Subpath)
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
	return EnsureVolume(ctx, c, home, repo.StorageVolume(p.ID), s, p.Subpath)
}

// ensureEverywhere makes a network share's volume on every ready node.
//
// Best effort per node: a node whose agent is down cannot take the
// definition, and failing the whole deploy for a machine the consumer may
// never land on trades a working deploy for a tidy one. The last error is
// only reported if no node took it.
func ensureEverywhere(ctx context.Context, c *cluster.Cluster, name string, s *repo.Storage, subpath string) (string, error) {
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	var got string
	var last error
	for _, n := range nodes {
		if n.Status() != "ready" {
			continue
		}
		v, err := EnsureVolume(ctx, c, n.ID, name, s, subpath)
		if err != nil {
			last = err
			continue
		}
		got = v
	}
	if got == "" {
		if last != nil {
			return "", fmt.Errorf("storage %s: no node could take it: %w", s.Slug, last)
		}
		return "", fmt.Errorf("storage %s: no ready node to mount it on", s.Slug)
	}
	return got, nil
}

// ParseAttachment decodes one tiles.storage line. It lives in placement so
// that package can tell which pools a tile attaches without importing this
// one, which would close a cycle through cluster.
var ParseAttachment = placement.ParseAttachment

// Resolve turns one attachment line into a ready bind string, ensuring the
// backing volume exists and applying forced-ro.
func Resolve(ctx context.Context, c *cluster.Cluster, store repo.Store, consumer *repo.Tile, line string) (string, error) {
	if varref.OrgStorageRef(line) != "" {
		return resolveOrgShare(ctx, c, store, consumer, line)
	}
	slug, pathName, mount, ro, err := ParseAttachment(line)
	if err != nil {
		return "", err
	}
	if pathName == "" {
		return "", fmt.Errorf("storage %q: source must be storage-slug/path-name", line)
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
		bind := name + ":" + mount + ":nocopy"
		if ro || paths[i].ForcedRO {
			bind += ",ro"
		}
		return bind, nil
	}
	return "", fmt.Errorf("storage %s has no declared sub-path %q; declare it first (strict sub-paths)", slug, pathName)
}

// resolveOrgShare mounts a sub-path of an org share:
// "${{ org.storage.NAME }}[/any/sub/path]:/mount[:ro]". Nothing is declared
// up front, so each distinct sub-path is its own volume, named from the path.
func resolveOrgShare(ctx context.Context, c *cluster.Cluster, store repo.Store, consumer *repo.Tile, line string) (string, error) {
	ex, err := varref.New(store).ExpandStrings(ctx, consumer.ID, varref.System, []string{line})
	if err != nil {
		return "", err
	}
	slug, sub, mount, ro, err := ParseAttachment(ex[0])
	if err != nil {
		return "", err
	}
	sub = strings.Trim(path.Clean("/"+sub), "/")
	for _, part := range strings.Split(sub, "/") {
		if part == ".." {
			return "", fmt.Errorf("storage %q: sub-path must stay inside the share", line)
		}
	}
	stack, err := store.GetStack(ctx, consumer.StackID)
	if err != nil || stack == nil {
		return "", fmt.Errorf("storage %q: stack not found", line)
	}
	s, err := store.GetOrgStorageBySlug(ctx, stack.OrgID, slug)
	if err != nil || s == nil {
		return "", fmt.Errorf("storage %q: share not found", line)
	}
	if err := ValidateAttach(s, consumer); err != nil {
		return "", err
	}
	name, err := ensureEverywhere(ctx, c, repo.OrgShareVolume(s, sub), s, sub)
	if err != nil {
		return "", err
	}
	bind := name + ":" + mount + ":nocopy"
	if ro {
		bind += ",ro"
	}
	return bind, nil
}
