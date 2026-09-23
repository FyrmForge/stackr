package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type storageStore struct {
	repo.Store
	bySlug  *repo.Storage
	created *repo.Storage
	paths   []repo.StoragePath
	tiles   []repo.Tile
}

func (s *storageStore) GetStorageBySlug(context.Context, string) (*repo.Storage, error) {
	return s.bySlug, nil
}
func (s *storageStore) GetOrgStorageBySlug(context.Context, string, string) (*repo.Storage, error) {
	return s.bySlug, nil
}
func (s *storageStore) CreateStorage(_ context.Context, st *repo.Storage) error {
	s.created = st
	return nil
}
func (s *storageStore) UpdateStorage(context.Context, *repo.Storage) error { return nil }
func (s *storageStore) ListTiles(context.Context) ([]repo.Tile, error)     { return s.tiles, nil }
func (s *storageStore) CreateStoragePath(_ context.Context, p *repo.StoragePath) error {
	s.paths = append(s.paths, *p)
	return nil
}
func (s *storageStore) DeleteStorage(context.Context, string) error { return nil }
func (s *storageStore) ListStoragePaths(context.Context, string) ([]repo.StoragePath, error) {
	return s.paths, nil
}

// The server id comes from the caller. Hard-coding "local" is why every pool
// a CLI created probed and mounted on the manager.
func TestCreateKeepsTheCallersServerID(t *testing.T) {
	st := &storageStore{}
	svc := NewStorageService(st, nil)
	if _, err := svc.Create(context.Background(), StorageSpec{
		Name: "Fast Pool", Backend: "local", Export: "/srv/fast", ServerID: "worker-2",
	}); err != nil {
		t.Fatal(err)
	}
	if st.created.ServerID != "worker-2" {
		t.Fatalf("server = %q", st.created.ServerID)
	}
	if st.created.Slug != "fast-pool" {
		t.Fatalf("slug = %q", st.created.Slug)
	}
}

// A punctuation-only sub-path name slugifies to "". The panel slugified and
// then checked, so it stored a sub-path called "".
func TestDeclarePathChecksAfterSlugifying(t *testing.T) {
	st := &storageStore{}
	svc := NewStorageService(st, nil)
	store := &repo.Storage{ID: "s1", Slug: "pool"}
	if _, err := svc.DeclarePath(context.Background(), store, "...", "sub", false); err == nil {
		t.Fatal(`a name of "..." was accepted`)
	}
	if _, err := svc.DeclarePath(context.Background(), store, "Cold Data", "../escape", false); err == nil {
		t.Fatal("a sub-path escaping the share was accepted")
	}
	p, err := svc.DeclarePath(context.Background(), store, "Cold Data", "/cold/", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "cold-data" || p.Subpath != "cold" {
		t.Fatalf("path %+v", p)
	}
}

// An org share is referenced as ${{ org.storage.NAME }}, which the slug/path
// scan cannot see. The panel's delete only had the scan, so deleting an org
// share that was still in use went through.
func TestConsumersSeesOrgShareReferences(t *testing.T) {
	st := &storageStore{tiles: []repo.Tile{
		{Name: "api", Storage: "${{ org.storage.media }}/uploads:/data"},
		{Name: "web", Storage: "pool/cold:/archive"},
	}}
	svc := NewStorageService(st, nil)
	ctx := context.Background()

	got, err := svc.Consumers(ctx, &repo.Storage{Slug: "media", OrgID: "org1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "api" {
		t.Fatalf("org share consumers = %v", got)
	}
	if got, _ = svc.Consumers(ctx, &repo.Storage{Slug: "pool"}, "cold"); len(got) != 1 || got[0] != "web" {
		t.Fatalf("pool consumers = %v", got)
	}
}
