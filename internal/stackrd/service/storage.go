package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// StorageService owns a share or pool and the sub-paths declared on it.
//
// The rules were split four ways. The API hard-coded `ServerID: "local"`,
// which is why a pool created from the CLI always probed and mounted on the
// manager rather than on the node it was meant for. The panel slugified a
// sub-path name and then checked it was non-empty, which passes a
// punctuation-only name straight through as `Name: ""`; the API checked first
// and slugified after, catching it. And the panel's delete had no org branch
// at all, so deleting an org share skipped the volume cleanup, skipped the
// managed-org check, and could not see the `${{ org.storage.NAME }}`
// references that are the only way an org share is ever attached.
type StorageService struct {
	store repo.Store
	clus  *cluster.Cluster
}

func NewStorageService(store repo.Store, clus *cluster.Cluster) *StorageService {
	return &StorageService{store: store, clus: clus}
}

// StorageSpec is a share or pool as a caller asks for it.
type StorageSpec struct {
	Name    string
	Backend string
	Address string
	Export  string
	// ServerID is the node the share is mounted on. It comes from the caller,
	// always: hard-coding it was why every CLI-created pool landed on the
	// manager. Empty only for an org share, which has no node of its own.
	ServerID string
	// OrgID makes it an org share rather than a server's pool.
	OrgID    string
	Username string
	Password string
	Opts     string
}

// Create validates, stores and probes. The probe is the point: a share that
// cannot be mounted should say so here rather than at the first deploy that
// needs it, and it runs on the share's own node — probing the manager proves
// nothing about the machine that will do the mounting.
func (s *StorageService) Create(ctx context.Context, in StorageSpec) (*repo.Storage, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || !storagetiles.ValidBackend(in.Backend) {
		return nil, invalid("", "name and a backend (nfs, smb or local) required")
	}
	st := &repo.Storage{
		ID: uuid.New().String(), ServerID: in.ServerID, OrgID: in.OrgID,
		Name: name, Slug: repo.Slugify(name), Backend: in.Backend,
		Address: strings.TrimSpace(in.Address), Export: strings.TrimSpace(in.Export),
		Username: in.Username, Password: in.Password, Opts: strings.TrimSpace(in.Opts),
		Status: "unknown", CreatedAt: time.Now().UTC(),
	}
	if st.Slug == "" {
		return nil, invalid("name", "a name needs at least one letter or number")
	}
	if st.Backend == "local" {
		if in.OrgID != "" {
			return nil, invalid("backend", "an org share is nfs or smb; local pools belong to a server")
		}
		if !strings.HasPrefix(st.Export, "/") {
			return nil, invalid("export", "a local pool needs an absolute host path")
		}
	} else if st.Address == "" {
		return nil, invalid("address", in.Backend+" storage needs an address")
	}
	if in.OrgID != "" {
		st.ServerID = ""
		if existing, _ := s.store.GetOrgStorageBySlug(ctx, in.OrgID, st.Slug); existing != nil {
			return nil, svcerr.Conflictf("storage %s already exists in this organization", st.Slug)
		}
	} else if existing, _ := s.store.GetStorageBySlug(ctx, st.Slug); existing != nil {
		return nil, svcerr.Conflictf("storage %s already exists", st.Slug)
	}
	if err := s.store.CreateStorage(ctx, st); err != nil {
		return nil, err
	}
	s.Probe(ctx, st)
	return st, nil
}

// Probe mounts the share once on its own node, records the result on the row
// and cleans the throwaway volume up. Best effort on the write: a probe
// result that fails to save is a stale badge, not a failed create.
func (s *StorageService) Probe(ctx context.Context, st *repo.Storage) {
	if s.clus == nil {
		return
	}
	node, err := s.clus.NodeOfStorage(ctx, st)
	if err != nil {
		st.Status, st.StatusMsg = "error", err.Error()
	} else {
		probe := repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID}
		if perr := storagetiles.Probe(ctx, s.clus, node, st, &probe); perr != nil {
			st.Status, st.StatusMsg = "error", perr.Error()
		} else {
			st.Status, st.StatusMsg = "ok", ""
		}
		_ = s.clus.RemoveVolume(ctx, node, repo.StorageVolume(probe.ID))
	}
	_ = s.store.UpdateStorage(ctx, st)
}

// Consumers names the tiles attached to this storage, optionally narrowed to
// one sub-path. An org share is referenced by `${{ org.storage.NAME }}` and a
// server's pool by `slug/path`, so both spellings are looked for — the
// panel's delete only ever looked for the second, which is why deleting an
// org share in use went through.
func (s *StorageService) Consumers(ctx context.Context, st *repo.Storage, pathName string) ([]string, error) {
	tiles, err := s.store.ListTiles(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range tiles {
		for _, l := range strings.Split(tiles[i].Storage, "\n") {
			l = strings.TrimSpace(l)
			if st.OrgID != "" {
				if varref.OrgStorageRef(l) == st.Slug {
					out = append(out, tiles[i].Name)
				}
				continue
			}
			slug, p, _, _, perr := storagetiles.ParseAttachment(l)
			if perr != nil || slug != st.Slug {
				continue
			}
			if pathName == "" || p == pathName {
				out = append(out, tiles[i].Name)
			}
		}
	}
	return out, nil
}

// Delete removes a share once nothing is attached to it. The data on the
// share itself is untouched; what goes is the row and the docker volumes
// stackr materialised for it.
func (s *StorageService) Delete(ctx context.Context, st *repo.Storage) error {
	if st == nil {
		return svcerr.ErrNotFound
	}
	users, err := s.Consumers(ctx, st, "")
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return svcerr.Conflictf("still attached to %s; detach first", strings.Join(users, ", "))
	}
	if s.clus != nil {
		if st.OrgID != "" {
			// An org share's volumes live on every node that mounted it, so
			// this is not the same cleanup as a pool's. The panel skipped it
			// entirely and left them behind.
			if err := storagetiles.DropOrgShareVolumes(ctx, s.clus, st); err != nil {
				return svcerr.Conflictf("%s", err.Error())
			}
		} else if node, nerr := s.clus.NodeOfStorage(ctx, st); nerr == nil {
			paths, _ := s.store.ListStoragePaths(ctx, st.ID)
			for i := range paths {
				_ = s.clus.RemoveVolume(ctx, node, repo.StorageVolume(paths[i].ID))
			}
		}
	}
	return s.store.DeleteStorage(ctx, st.ID)
}

// DeclarePath adds a sub-path and materialises it, so a typo fails here
// rather than at the first deploy that mounts it.
//
// The name is slugified and *then* checked for emptiness. The panel did it
// the other way round, so a name of "..." became a sub-path called "".
func (s *StorageService) DeclarePath(ctx context.Context, st *repo.Storage, name, subpath string, forcedRO bool) (*repo.StoragePath, error) {
	if st == nil {
		return nil, svcerr.ErrNotFound
	}
	if st.OrgID != "" {
		return nil, invalid("", "an org share takes any sub-path; mount ${{ org.storage."+st.Slug+" }}/sub/path directly")
	}
	slug := repo.Slugify(strings.TrimSpace(name))
	if slug == "" {
		return nil, invalid("name", "a sub-path name needs at least one letter or number")
	}
	sub := strings.Trim(strings.TrimSpace(subpath), "/")
	if strings.Contains(sub, "..") {
		return nil, invalid("subpath", "sub-path must stay inside the share")
	}
	p := &repo.StoragePath{ID: uuid.New().String(), StorageID: st.ID, Name: slug,
		Subpath: sub, ForcedRO: forcedRO, CreatedAt: time.Now().UTC()}
	if err := s.store.CreateStoragePath(ctx, p); err != nil {
		return nil, err
	}
	if s.clus == nil {
		return p, nil
	}
	node, err := s.clus.NodeOfStorage(ctx, st)
	if err != nil {
		return p, svcerr.Invalidf("", "sub-path declared, but %s", err.Error())
	}
	if err := storagetiles.Probe(ctx, s.clus, node, st, p); err != nil {
		return p, svcerr.Invalidf("", "sub-path declared but mounting it failed: %s", err.Error())
	}
	return p, nil
}
