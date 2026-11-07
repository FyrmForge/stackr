package service

import (
	"context"
	"testing"
)

// A slug the registry still holds images under cannot be taken by a rename or
// a new org: registry rights follow the slug, not the org.
func TestRenameOrgRefusesSlugHeldByRegistry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	w.fakeRegistry(t, map[string][]string{"old-co/docker.io/library/app": {"1"}, "emptied/app": {}})

	if _, _, err := w.orch.RenameOrg(ctx, w.org, "Old Co"); err == nil {
		t.Fatal("renamed onto a slug the registry holds images under")
	}
	if og, _ := w.orch.orgs.Get(ctx, w.org); og.Slug != "acme" {
		t.Errorf("slug = %q after a refused rename", og.Slug)
	}
	if _, _, err := w.orch.RenameOrg(ctx, w.org, "Emptied"); err != nil {
		t.Errorf("a slug with only emptied repositories = %v", err)
	}
}
