package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/deploystate"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type regStore struct {
	repo.Store
	reg     *repo.Registry
	deleted []string
	stacks  []repo.Stack
	tiles   []repo.Tile
	deps    []repo.Deployment
}

func (r *regStore) GetRegistry(context.Context, string) (*repo.Registry, error) { return r.reg, nil }
func (r *regStore) DeleteRegistry(_ context.Context, id string) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (r *regStore) ListStacksByOrg(context.Context, string) ([]repo.Stack, error) {
	return r.stacks, nil
}
func (r *regStore) ListTilesByStack(context.Context, string) ([]repo.Tile, error) {
	return r.tiles, nil
}
func (r *regStore) ListDeploymentsByTile(context.Context, string, int) ([]repo.Deployment, error) {
	return r.deps, nil
}

// The panel's delete took an id and ran. One POST removed the managed
// registry; boot recreated it with a fresh password and every org's derived
// credential stopped working.
func TestDeleteRefusesTheManagedRegistry(t *testing.T) {
	st := &regStore{reg: &repo.Registry{ID: "r1", Managed: true}}
	svc := NewRegistryService(st)
	var conflict svcerr.Conflict
	if err := svc.Delete(context.Background(), "r1"); !errors.As(err, &conflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
	if len(st.deleted) != 0 {
		t.Fatal("the managed registry was deleted anyway")
	}
	st.reg.Managed = false
	if err := svc.Delete(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
}

// One in-use matcher. The panel's split the stored tag at its first slash and
// indexed by the tail, so a tag stored without a pull host matched nothing
// and was deletable while a live deployment still pointed at it.
func TestTagInUseMatchesAHostlessStoredTag(t *testing.T) {
	org := &repo.Org{ID: "o1", Slug: "acme"}
	st := &regStore{
		stacks: []repo.Stack{{ID: "s1", Slug: "shop"}},
		tiles:  []repo.Tile{{ID: "t1", Slug: "api"}},
	}
	svc := NewRegistryService(st)
	ctx := context.Background()

	st.deps = []repo.Deployment{{Status: deploystate.Done, ImageTag: "acme_api:v1"}}
	where, err := svc.TagInUse(ctx, org, "acme_api", "v1")
	if err != nil || where != "shop/api" {
		t.Fatalf("hostless tag: %q %v", where, err)
	}
	st.deps = []repo.Deployment{{Status: deploystate.Done, ImageTag: "reg.example.com/acme_api:v1"}}
	if where, _ = svc.TagInUse(ctx, org, "acme_api", "v1"); where != "shop/api" {
		t.Fatalf("host-prefixed tag: %q", where)
	}
	// A failed deployment does not hold a tag.
	st.deps = []repo.Deployment{{Status: deploystate.Error, ImageTag: "acme_api:v1"}}
	if where, _ = svc.TagInUse(ctx, org, "acme_api", "v1"); where != "" {
		t.Fatalf("a failed deployment held the tag: %q", where)
	}
	// And a tag that is not a tag is refused rather than scanned.
	if _, err := svc.TagInUse(ctx, org, "acme_api", "not a tag"); err == nil {
		t.Fatal("a malformed tag was accepted")
	}
}

// A short name is the friendly form the listing shows and a full one is what
// a script copies out of an image reference. Neither may reach outside the
// org's namespace.
func TestImageStaysInTheOrgNamespace(t *testing.T) {
	svc := NewRegistryService(&regStore{})
	org := &repo.Org{ID: "o1", Slug: "acme"}
	ctx := context.Background()

	// The namespace is "<org>_": slugs are [a-z0-9-] only, so the trailing
	// underscore is an unambiguous boundary.
	full, err := svc.Image(ctx, org, "api")
	if err != nil || full != "acme_api" {
		t.Fatalf("short name gave %q %v", full, err)
	}
	if full, err = svc.Image(ctx, org, "acme_api"); err != nil || full != "acme_api" {
		t.Fatalf("full name gave %q %v", full, err)
	}
	if _, err := svc.Image(ctx, org, "../other/api"); !errors.Is(err, svcerr.ErrNotFound) {
		t.Fatalf("an escaping name gave %v", err)
	}
}
