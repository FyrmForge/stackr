package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Remove's two refusals come before anything touches the swarm, which is what
// makes them the two worth a test: they are the guards a node API would have
// to re-derive, and the second one is the only thing standing between a click
// and a machine's volumes becoming unreachable.
func TestRemoveRefusesTheManagerAndAnUnconfirmedName(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	testdb.SeedStack(t, store, false)
	svc := service.NewNodeService(store, nil, "")

	mgr := &repo.Server{ID: "local", Name: "manager", Role: "manager"}
	inv, ok := svcerr.IsInvalid(svc.Remove(ctx, mgr, true))
	require.True(t, ok)
	assert.Contains(t, inv.Msg, "cannot be removed")

	worker := &repo.Server{ID: "n2", Name: "worker-2", Role: "worker"}
	inv, ok = svcerr.IsInvalid(svc.Remove(ctx, worker, false))
	require.True(t, ok)
	assert.Equal(t, "type worker-2 to confirm: removing it leaves the volumes on that machine unreachable", inv.Error(),
		"the whole sentence, no field prefix: both surfaces render it verbatim")
}

// A node that never joined has no swarm object to label, so the group write
// has nothing to write to.
func TestSetGroupNeedsAJoinedNode(t *testing.T) {
	store := testdb.New(t)
	svc := service.NewNodeService(store, nil, "")
	_, ok := svcerr.IsInvalid(svc.SetGroup(context.Background(), &repo.Server{ID: "n2"}, "ssd"))
	assert.True(t, ok)
}
