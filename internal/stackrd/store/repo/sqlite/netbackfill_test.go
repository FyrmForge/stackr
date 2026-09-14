package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Migration 009 added environments.network with an empty default and
// backfilled nothing, and only a deploy ever fills it. So every environment
// that existed before that upgrade sat on no overlay indefinitely: port
// forward refused, and the proxy left the whole environment out of its
// routing map without saying so.
//
// netpool.Backfill closes that at boot, and this is the query it walks.
func TestEnvironmentsWithoutNetworkFindsExactlyTheUnclaimedOnes(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	now := time.Now().UTC()

	org := &repo.Org{ID: "org1", Name: "Org", Slug: "org", CreatedAt: now, SetupDoneAt: &now}
	require.NoError(t, store.CreateOrg(ctx, org))
	st := &repo.Stack{ID: "stack1", OrgID: org.ID, Name: "S", Slug: "s", CreatedAt: now}
	require.NoError(t, store.CreateStack(ctx, st))

	// Two upgraded environments with nothing claimed, one already on an
	// overlay. Only the first two are the migration's leftovers.
	for _, e := range []struct {
		id, slug, network string
	}{
		{"env-old-1", "prod", ""},
		{"env-old-2", "staging", ""},
		{"env-new", "dev", "stkr-net-07"},
	} {
		env := &repo.Environment{ID: e.id, StackID: st.ID, Name: e.slug, Slug: e.slug,
			Type: "static", Network: e.network, CreatedAt: now}
		require.NoError(t, store.CreateEnvironment(ctx, env), "create %s", e.id)
	}

	got, err := store.EnvironmentsWithoutNetwork(ctx)
	require.NoError(t, err)

	// Not an exact set: CreateStack makes a Production environment of its own,
	// which legitimately has no network yet either.
	assert.Contains(t, got, "env-old-1")
	assert.Contains(t, got, "env-old-2")
	assert.NotContains(t, got, "env-new",
		"an environment already holding an overlay must not be re-claimed: that would leak a pool entry")
}
