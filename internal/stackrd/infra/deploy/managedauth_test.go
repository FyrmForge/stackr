package deploy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Only the build path ever carried a credential, through pushToRegistry. A
// tile with an `image:` in this install's own registry, and every promote or
// rollback of a tag the registry holds, pulled anonymously: the manager 401ed,
// and where it already held the image the spec reached the workers with no
// credential at all and their tasks never placed.
//
// No registry is configured here, so OrgPullAuth cannot resolve one and every
// case answers "". What this pins is the host arithmetic in front of it: a
// public image must never be handed a credential, whatever the registry says.
func TestManagedAuthOnlyForARegistryHost(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	e := NewEngine(store, nil, nil, nil, t.TempDir(), nil, storeRows{s: store}, nil)

	// No host segment at all: docker hub, and a credential must not travel.
	for _, ref := range []string{"nginx:1.27", "alpine", "library/nginx:1.27", ""} {
		assert.Empty(t, e.managedAuth(ctx, seed.Tile, ref), "credential offered to %q", ref)
	}
	// A host segment, but no managed registry on this install to match it.
	for _, ref := range []string{"10.0.0.10:5000/demo_shop_api:v1", "ghcr.io/acme/x:1"} {
		assert.Empty(t, e.managedAuth(ctx, seed.Tile, ref),
			"credential invented for %q with no registry configured", ref)
	}
	// A tile whose stack is gone resolves nothing rather than panicking.
	assert.Empty(t, e.managedAuth(ctx, &repo.Tile{ID: "x", Slug: "x", StackID: "nope"},
		"10.0.0.10:5000/demo_x:v1"))
}
