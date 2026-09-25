package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// FinishOrg gives an org with no resource of its own one undeclared
// <slug>.<instance host> row; a second finish adds nothing, and with no
// instance row there is nothing to name it under.
func TestFinishOrgDefaultDomain(t *testing.T) {
	ctx := context.Background()

	bare := newWorld(t)
	_, err := bare.orch.FinishOrg(ctx, bare.org)
	must(t, err)
	rows, err := bare.orch.AllDomainResources(ctx)
	must(t, err)
	if len(rows) != 0 {
		t.Fatalf("no instance row: rows = %+v, want none", rows)
	}

	w := newWorld(t)
	must(t, w.orch.domainres.SeedInstance(ctx, "example.com"))
	for range 2 {
		_, err := w.orch.FinishOrg(ctx, w.org)
		must(t, err)
	}
	rows, err = w.orch.DomainResources(ctx, w.org)
	must(t, err)
	if len(rows) != 2 || rows[0].Level != domainres.Org || rows[0].Host != "acme.example.com" || rows[0].Declared {
		t.Errorf("after two finishes = %+v, want one undeclared acme.example.com org row before the instance row", rows)
	}
}

// A hand-attached host leading with another org's slug is refused.
func TestAttachDomainSquat(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.st.Orgs.Create(ctx, store.Org{
		ID:        uuid.NewString(),
		Name:      "globex",
		Slug:      "globex",
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: time.Now(),
	}))
	api := w.tile(t, "api", false)
	_, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "globex.example.com"})
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("squatting attach = %v, want a conflict", err)
	}
	_, err = w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "acme.example.com"})
	must(t, err)
}

// A managed tile's public base is its auto name under the nearest visible
// resource, once a domain routes that name to it; "" before.
func TestManagedPublicBase(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.orch.domainres.SeedInstance(ctx, "example.com"))
	s3 := store.Tile{
		ID:            uuid.NewString(),
		StackID:       w.stack,
		EnvironmentID: w.env,
		Name:          "files",
		Slug:          "files",
		Kind:          tile.Managed,
		EnvJSON:       "{}",
		BuildArgs:     "{}",
		Replicas:      1,
		UpdatePolicy:  "manual",
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	must(t, w.st.Tiles.Create(ctx, s3))
	if got := w.orch.publicBase(ctx, s3); got != "" {
		t.Fatalf("unrouted public base = %q, want empty", got)
	}
	// dev is the stack's only env, so the default: no env label.
	_, err := w.orch.AttachDomain(ctx, s3.ID, DomainSpec{Host: "files.shop.acme.example.com", Port: 9000})
	must(t, err)
	if got := w.orch.publicBase(ctx, s3); got != "https://files.shop.acme.example.com" {
		t.Errorf("public base = %q, want https://files.shop.acme.example.com", got)
	}
}

// A resource's ACME email reaches the pushed proxy config for the hosts
// under it; while it names tile domains a delete is refused with the count,
// and an unused one goes.
func TestDeleteDomainResource(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	r, err := w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "acme.io", false, "ops@acme.io")
	must(t, err)
	api := w.tile(t, "api", false)
	must(t, w.st.Domains.Create(ctx, domainRow(api.ID, "api.acme.io", &r.ID)))
	must(t, w.st.Domains.Create(ctx, domainRow(api.ID, "www.acme.io", &r.ID)))
	must(t, w.orch.SyncProxy(ctx))
	if !strings.Contains(w.lastPush(), "ops@acme.io") {
		t.Fatalf("pushed config lacks the resource's ACME email:\n%s", w.lastPush())
	}
	err = w.orch.DeleteDomainResource(ctx, r.ID)
	if c, ok := errs.IsConflict(err); !ok || !strings.Contains(c.Msg, "2 tile domains") {
		t.Fatalf("delete while named = %v, want a conflict counting 2", err)
	}

	unused, err := w.orch.CreateDomainResource(ctx, domainres.Org, w.org, "acme.dev", false, "")
	must(t, err)
	must(t, w.orch.DeleteDomainResource(ctx, unused.ID))
}

// domainRow is a tile domain a promote wrote under resource resID.
func domainRow(tileID, host string, resID *string) store.Domain {
	return store.Domain{
		ID:            uuid.NewString(),
		TileID:        tileID,
		Host:          host,
		ContainerPort: 80,
		HTTPS:         true,
		ForceHTTPS:    true,
		Auto:          true,
		ResourceID:    resID,
		ProxyJSON:     "{}",
		CreatedAt:     time.Now(),
	}
}
