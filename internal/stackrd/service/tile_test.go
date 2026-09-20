package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// deleteStore answers GetStack for the gate and nothing else. Every case
// below must refuse before the teardown starts, so a store call past that
// point panics on the nil embedded interface — which is the assertion.
type deleteStore struct {
	repo.Store
	stack   *repo.Stack
	owner   *repo.Tile // what the volume's attach id resolves to
	deleted *string    // set when DeleteTile runs
}

func (d deleteStore) GetStack(context.Context, string) (*repo.Stack, error) { return d.stack, nil }

func (d deleteStore) GetTile(context.Context, string) (*repo.Tile, error) { return d.owner, nil }

func (d deleteStore) ListProvisionsByConsumer(context.Context, string) ([]repo.Provision, error) {
	return nil, nil
}

func (d deleteStore) DeleteTile(_ context.Context, id string) error {
	if d.deleted != nil {
		*d.deleted = id
	}
	return nil
}

func deleteSvc(st repo.Store) *TileService {
	return NewTileService(st, nil, nil, nil, nil, nil, NewGateService(st))
}

// The file owns the shape of the stack, so a delete refuses whatever
// ui_edits says — and it refuses before anything is torn down.
func TestDeleteRefusesOnAConfigManagedStack(t *testing.T) {
	st := deleteStore{stack: &repo.Stack{ID: "s1", ConfigConnectorID: "c", ConfigRepo: "acme/infra",
		UIEditsMode: repo.UIEditsStage}}
	_, err := deleteSvc(st).Delete(context.Background(),
		&repo.Tile{ID: "t1", StackID: "s1", Kind: "service"}, Actor{Via: "api"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
}

// The guard the API skipped entirely: deleting an attached volume took it out
// from under a running service. It must fire before the teardown, and on
// both surfaces.
func TestDeleteRefusesAnAttachedVolume(t *testing.T) {
	st := deleteStore{stack: &repo.Stack{ID: "s1"}, owner: &repo.Tile{ID: "t9", Slug: "api"}}
	vol := &repo.Tile{ID: "v1", StackID: "s1", Kind: "volume", AttachedTileID: "t9"}
	for _, via := range []string{"api", "web"} {
		_, err := deleteSvc(st).Delete(context.Background(), vol, Actor{Via: via})
		inv, ok := svcerr.IsInvalid(err)
		if !ok {
			t.Fatalf("via %s: want an invalid, got %v (%T)", via, err, err)
		}
		if inv.Msg == "" {
			t.Errorf("via %s: the refusal has to say what to do about it", via)
		}
	}
}

func TestDeleteOfNothingIsNotFound(t *testing.T) {
	if _, err := deleteSvc(deleteStore{}).Delete(context.Background(), nil, Actor{}); err != svcerr.ErrNotFound {
		t.Errorf("got %v", err)
	}
}

// The other side of the same guard: a volume whose owner was deleted keeps a
// stale attach id, and refusing on the id alone left it undeletable for ever.
// The rig found this one — the tile row does not cascade to its volumes.
func TestDeleteAllowsAVolumeWhoseOwnerIsGone(t *testing.T) {
	var gone string
	st := deleteStore{stack: &repo.Stack{ID: "s1"}, deleted: &gone} // GetTile answers nil
	vol := &repo.Tile{ID: "v1", StackID: "s1", Kind: "volume", AttachedTileID: "gone"}
	if _, err := deleteSvc(st).Delete(context.Background(), vol, Actor{Via: "api"}); err != nil {
		t.Fatalf("an orphaned volume must be deletable: %v", err)
	}
	if gone != "v1" {
		t.Errorf("the row was not deleted (DeleteTile saw %q)", gone)
	}
}

// orphanStore is a whole environment: the tile being torn down plus the
// volumes attached to it.
type orphanStore struct {
	repo.Store
	stack   *repo.Stack
	tiles   []repo.Tile
	updated []repo.Tile
}

func (o *orphanStore) GetStack(context.Context, string) (*repo.Stack, error) { return o.stack, nil }
func (o *orphanStore) GetTile(_ context.Context, id string) (*repo.Tile, error) {
	for i := range o.tiles {
		if o.tiles[i].ID == id {
			return &o.tiles[i], nil
		}
	}
	return nil, nil
}
func (o *orphanStore) ListTilesByEnv(context.Context, string) ([]repo.Tile, error) {
	return o.tiles, nil
}
func (o *orphanStore) ListProvisionsByConsumer(context.Context, string) ([]repo.Provision, error) {
	return nil, nil
}
func (o *orphanStore) DeleteTile(context.Context, string) error { return nil }
func (o *orphanStore) UpdateTile(_ context.Context, id string, cfg repo.TileConfig) error {
	o.updated = append(o.updated, repo.Tile{ID: id, TileConfig: cfg})
	return nil
}

// Deleting a service orphans its volumes. The tile row does not cascade to
// them, so the attach pointer used to outlive its target: the volume then
// read as attached to nothing resolvable, and Delete's own attached-guard
// could not tell that from a live owner, which left it undeletable.
func TestTearDownOrphansVolumes(t *testing.T) {
	st := &orphanStore{
		stack: &repo.Stack{ID: "s1"},
		tiles: []repo.Tile{
			{ID: "api", StackID: "s1", EnvironmentID: "e1", Kind: "service"},
			{ID: "data", StackID: "s1", EnvironmentID: "e1", Kind: "volume",
				AttachedTileID: "api", MountPath: "/data"},
			{ID: "other", StackID: "s1", EnvironmentID: "e1", Kind: "volume",
				AttachedTileID: "web", MountPath: "/srv"},
		},
	}
	svc := NewTileService(st, nil, nil, nil, nil, nil, NewGateService(st))
	if err := svc.TearDown(context.Background(), &st.tiles[0]); err != nil {
		t.Fatal(err)
	}
	if len(st.updated) != 1 {
		t.Fatalf("updated %d tiles, want only the one attached to it", len(st.updated))
	}
	got := st.updated[0]
	if got.ID != "data" || got.AttachedTileID != "" || got.MountPath != "" {
		t.Fatalf("orphaned %+v", got)
	}
}
