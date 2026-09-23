package service

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// tenancyStore is the subtree one org owns, plus a second org's stack to
// prove the walk lands on the right one.
type tenancyStore struct {
	repo.Store
	boom bool
}

func (s *tenancyStore) fail() error {
	if s.boom {
		return errors.New("db is down")
	}
	return nil
}

func (s *tenancyStore) GetOrg(_ context.Context, id string) (*repo.Org, error) {
	if id == "org1" || id == "org2" {
		return &repo.Org{ID: id}, s.fail()
	}
	return nil, s.fail()
}
func (s *tenancyStore) GetOrgBySlug(_ context.Context, slug string) (*repo.Org, error) {
	if slug == "acme" {
		return &repo.Org{ID: "org1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetStack(_ context.Context, id string) (*repo.Stack, error) {
	switch id {
	case "stack1":
		return &repo.Stack{ID: "stack1", OrgID: "org1"}, s.fail()
	case "stack2":
		return &repo.Stack{ID: "stack2", OrgID: "org2"}, nil
	}
	return nil, s.fail()
}
func (s *tenancyStore) GetStackBySlug(_ context.Context, orgID, slug string) (*repo.Stack, error) {
	if orgID == "org1" && slug == "shop" {
		return &repo.Stack{ID: "stack1", OrgID: "org1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetEnvironment(_ context.Context, id string) (*repo.Environment, error) {
	if id == "env1" {
		return &repo.Environment{ID: "env1", StackID: "stack1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetEnvironmentBySlug(_ context.Context, stackID, slug string) (*repo.Environment, error) {
	if stackID == "stack1" && slug == "prod" {
		return &repo.Environment{ID: "env1", StackID: "stack1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetTile(_ context.Context, id string) (*repo.Tile, error) {
	if id == "tile1" {
		return &repo.Tile{ID: "tile1", StackID: "stack1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetTileBySlug(_ context.Context, envID, slug string) (*repo.Tile, error) {
	if envID == "env1" && slug == "api" {
		return &repo.Tile{ID: "tile1", StackID: "stack1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetBackup(_ context.Context, id string) (*repo.Backup, error) {
	if id == "b1" {
		return &repo.Backup{ID: "b1", TileID: sql.NullString{String: "tile1", Valid: true}}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetDeployment(_ context.Context, id string) (*repo.Deployment, error) {
	if id == "d1" {
		return &repo.Deployment{ID: "d1", TileID: "tile1"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetConfigPlan(_ context.Context, id string) (*repo.ConfigPlan, error) {
	if id == "p1" {
		return &repo.ConfigPlan{ID: "p1", StackID: "stack2"}, nil
	}
	return nil, nil
}
func (s *tenancyStore) GetBackupDestination(_ context.Context, id string) (*repo.BackupDestination, error) {
	switch id {
	case "dest-org":
		return &repo.BackupDestination{ID: id, OrgID: sql.NullString{String: "org1", Valid: true}}, nil
	case "dest-server":
		return &repo.BackupDestination{ID: id}, nil // NULL org: the admin's
	}
	return nil, nil
}
func (s *tenancyStore) ListDomainResources(context.Context) ([]repo.DomainResource, error) {
	return []repo.DomainResource{
		{ID: "dr-org", Level: "org", OwnerID: "org2"},
		{ID: "dr-stack", Level: "stack", OwnerID: "stack1"},
		{ID: "dr-inst", Level: "instance", OwnerID: "local"},
	}, nil
}

func tenancySvc() *AccessService { return NewAccessService(&tenancyStore{}) }

// Every kind walks up to the org that owns it. The interesting ones are the
// indirect walks: a backup reaches its org through its tile through its
// stack, and each of the three gate helpers that do this today walks it in a
// different shape.
func TestTenancyOfWalksEveryKindUpToItsOrg(t *testing.T) {
	ctx := context.Background()
	s := tenancySvc()
	for _, tc := range []struct {
		kind Kind
		ref  string
		want string
	}{
		{KindOrg, "org1", "org1"},
		{KindStack, "stack1", "org1"},
		{KindEnv, "env1", "org1"},
		{KindTile, "tile1", "org1"},
		{KindBackup, "b1", "org1"},
		{KindDeployment, "d1", "org1"},
		{KindStackPlan, "p1", "org2"},
		{KindDestination, "dest-org", "org1"},
		{KindDomainResource, "dr-org", "org2"},
		{KindDomainResource, "dr-stack", "org1"},
	} {
		got, err := s.TenancyOf(ctx, tc.kind, tc.ref)
		if err != nil {
			t.Errorf("%s %s: %v", tc.kind, tc.ref, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s %s resolved to %q, wanted %q", tc.kind, tc.ref, got, tc.want)
		}
	}
}

// A slug path resolves to the same org its id does, at every depth. An id is
// tried first, so this only runs when the id lookup misses.
func TestTenancyOfAcceptsSlugPaths(t *testing.T) {
	ctx := context.Background()
	s := tenancySvc()
	for _, ref := range []struct {
		kind Kind
		path string
	}{
		{KindOrg, "acme"},
		{KindStack, "acme:shop"},
		{KindEnv, "acme:shop:prod"},
		{KindTile, "acme:shop:prod:api"},
	} {
		got, err := s.TenancyOf(ctx, ref.kind, ref.path)
		if err != nil || got != "org1" {
			t.Errorf("%s %q resolved to %q, %v", ref.kind, ref.path, got, err)
		}
	}
}

// A bare slug below org level names no tenant. Slugs repeat across orgs, so
// resolving one would have to guess which org's "shop" was meant, and a guess
// here edits the wrong tenant's stack.
func TestTenancyOfRefusesATenantlessSlug(t *testing.T) {
	ctx := context.Background()
	s := tenancySvc()
	for _, ref := range []string{"shop", "prod", "api", "acme:shop:prod:api:extra", ""} {
		if got, _ := s.TenancyOf(ctx, KindStack, ref); got != "" {
			t.Errorf("%q resolved to %q; a slug with no org is not addressable", ref, got)
		}
	}
}

// Things that are not org content resolve to no org, and that is the answer
// rather than a failure: their verb is admin-level, and Require never looks
// at the org for one.
func TestTenancyOfReturnsNoOrgForServerOwnedThings(t *testing.T) {
	ctx := context.Background()
	s := tenancySvc()
	for _, tc := range []struct {
		kind Kind
		ref  string
	}{
		{KindNone, "anything"},
		{KindDestination, "dest-server"}, // NULL org_id: the admin's
		{KindDomainResource, "dr-inst"},  // instance level
	} {
		got, err := s.TenancyOf(ctx, tc.kind, tc.ref)
		if err != nil || got != "" {
			t.Errorf("%s %s resolved to %q, %v; wanted no org", tc.kind, tc.ref, got, err)
		}
	}
}

// A missing row is ("", nil), not an error: the surface decides whether that
// reads as 404 or 403, and those are deliberately different.
func TestTenancyOfTreatsAMissingRowAsNoOrg(t *testing.T) {
	ctx := context.Background()
	s := tenancySvc()
	for _, k := range []Kind{KindOrg, KindStack, KindEnv, KindTile, KindBackup, KindStackPlan} {
		got, err := s.TenancyOf(ctx, k, "nope")
		if err != nil || got != "" {
			t.Errorf("%s: got %q, %v", k, got, err)
		}
	}
}

// A store error is an error, never a silent "no org" — which would read as
// "not org content" and hand the route to the admin check.
func TestTenancyOfPropagatesStoreErrors(t *testing.T) {
	s := NewAccessService(&tenancyStore{boom: true})
	if _, err := s.TenancyOf(context.Background(), KindStack, "stack1"); err == nil {
		t.Fatal("a store failure resolved as a clean lookup")
	}
}
