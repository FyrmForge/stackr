package nodes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// Swarm issues a new node ID on every join, so a node removed and rejoined
// leaves its row pointing at an ID that no longer exists. match used to give
// up there and report the row pending, which it then stayed for good: the
// join script it offered could never resolve, and Sync's adoption loop is
// disabled while anything is pending, so a hand-joined node could not be
// picked up either.
func TestMatchReattachesARowWhoseNodeRejoined(t *testing.T) {
	store := testdb.New(t)
	s := &Service{Store: store, Rows: testdb.NodeRows{Store: store}}
	ctx := context.Background()

	sv := &repo.Server{
		ID: "srv-1", Name: "worker-2", Kind: "swarm",
		NodeID:  "0txxsmu7xpujsrjkf9asgeijq", // the id from a previous join
		Address: "10.0.0.20", Hostname: "worker-2.example.test",
		Role: "worker", Status: "ready", Settings: "{}",
	}
	require.NoError(t, store.CreateServer(ctx, sv))

	// Swarm knows the same machine under a new id.
	fresh := runtime.Node{
		ID: "ufup1iv9z8vnpj2yubolb9ga6", Hostname: "worker-2.example.test",
		Addr: "10.0.0.20", Role: "worker", State: "ready", Availability: "active",
	}
	byID := map[string]runtime.Node{fresh.ID: fresh}

	got := s.match(ctx, sv, byID, map[string]bool{})
	require.NotNil(t, got, "a rejoined node must re-attach to its existing row, not read pending for good")
	assert.Equal(t, fresh.ID, got.ID)

	stored, err := store.GetServer(ctx, "srv-1")
	require.NoError(t, err)
	assert.Equal(t, fresh.ID, stored.NodeID, "the new node id must be mirrored onto the row")
}

// One swarm node belongs to exactly one row. Making match fall through on a
// stale ID opened a way for two rows to claim the same node: the row with the
// stale ID re-attaches by address, while a second row that was already
// mirrored still holds that node's ID and matches it straight from byID.
//
// The damage is not cosmetic. GetServerByNodeID then answers with whichever
// row the database reaches first, so the agent's host samples land under one
// server ID or the other at random, and Remove on either row drains the one
// real machine.
func TestMatchRefusesToClaimANodeAnotherRowAlreadyHas(t *testing.T) {
	store := testdb.New(t)
	s := &Service{Store: store, Rows: testdb.NodeRows{Store: store}}
	ctx := context.Background()

	node := runtime.Node{
		ID: "ufup1iv9z8vnpj2yubolb9ga6", Hostname: "worker-2.example.test",
		Addr: "10.0.0.20", Role: "worker", State: "ready", Availability: "active",
	}
	byID := map[string]runtime.Node{node.ID: node}
	claimed := map[string]bool{}

	// The row holding a stale id re-attaches by address and takes the node.
	stale := &repo.Server{
		ID: "srv-old", Name: "worker-2", Kind: "swarm",
		NodeID: "0txxsmu7xpujsrjkf9asgeijq", Address: "10.0.0.20",
		Hostname: "worker-2.example.test", Role: "worker", Status: "ready", Settings: "{}",
	}
	require.NoError(t, store.CreateServer(ctx, stale))
	require.NotNil(t, s.match(ctx, stale, byID, claimed), "the stale row re-attaches")

	// A duplicate row that was already mirrored still names the same node.
	dup := &repo.Server{
		ID: "srv-dup", Name: "worker-2", Kind: "swarm",
		NodeID: node.ID, Address: "10.0.0.20",
		Hostname: "worker-2.example.test", Role: "worker", Status: "ready", Settings: "{}",
	}
	require.NoError(t, store.CreateServer(ctx, dup))
	assert.Nil(t, s.match(ctx, dup, byID, claimed),
		"a node already claimed by another row must not be claimed twice")
}

// Two rows for one machine is not a cosmetic duplicate: both compete for the
// same swarm node, only one can claim it, and the loser reads pending for
// good while Sync's adoption loop stays disabled behind it.
func TestAddNodeRefusesAnAddressAlreadyAdded(t *testing.T) {
	store := testdb.New(t)
	s := &Service{Store: store, Rows: testdb.NodeRows{Store: store}}
	ctx := context.Background()

	sv, key, err := s.AddNode(ctx, "worker-2", "10.0.0.20")
	require.NoError(t, err, "the first add works")
	require.NotNil(t, sv)
	require.NotNil(t, key)

	_, _, err = s.AddNode(ctx, "worker-2-again", "10.0.0.20")
	require.Error(t, err, "the same address must not produce a second row")
	assert.Contains(t, err.Error(), "worker-2", "the message names the row that already holds it")

	rows, err := store.ListServers(ctx)
	require.NoError(t, err)
	n := 0
	for i := range rows {
		if rows[i].Address == "10.0.0.20" {
			n++
		}
	}
	assert.Equal(t, 1, n, "exactly one row per address")

	// A different address is still fine.
	_, _, err = s.AddNode(ctx, "worker-2", "192.168.1.108")
	assert.NoError(t, err)
}
