package nodes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The rails Claim checks before it ever asks swarm anything. The rest of it
// (the id is really a node, and no other row holds it) needs a live swarm and
// is rig work: rt.ListNodes has no fake.
func TestClaimRefusesAKeyItShouldNot(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	s := &Service{Store: store, Rows: testdb.NodeRows{Store: store}}

	sv := &repo.Server{ID: "srv-1", Name: "worker-2", Kind: "swarm", Address: "10.0.0.20",
		Role: "worker", Status: "pending", Settings: "{}", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateServer(ctx, sv), "seed server")

	live := &repo.JoinKey{Key: "live", ServerID: sv.ID, Address: sv.Address,
		ExpiresAt: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateJoinKey(ctx, live), "seed key")
	old := &repo.JoinKey{Key: "old", ServerID: sv.ID, Address: sv.Address,
		ExpiresAt: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateJoinKey(ctx, old), "seed expired key")

	assert.ErrorContains(t, s.Claim(ctx, "live", "", sv.Address), "no node id")
	assert.ErrorContains(t, s.Claim(ctx, "nope", "nodeid1", sv.Address), "unknown join key")
	assert.ErrorContains(t, s.Claim(ctx, "old", "nodeid1", sv.Address), "expired")
	// A key that left the private network entirely. A different private
	// address is allowed: behind NAT that is every ordinary join.
	assert.ErrorContains(t, s.Claim(ctx, "live", "nodeid1", "203.0.113.9"),
		"outside your private network")

	// None of those wrote anything.
	got, err := store.GetServer(ctx, sv.ID)
	require.NoError(t, err)
	assert.Empty(t, got.NodeID, "a refused claim still set the node id")
}

// The join script is a root shell script and renders the operator's name in a
// comment line, so a newline in it adds lines to the script. Refused at the
// writer every caller routes through.
func TestAddNodeRefusesALineBreakInTheName(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	s := &Service{Store: store, Rows: testdb.NodeRows{Store: store}}

	sv, key, err := s.AddNode(ctx, "web\necho hi", "192.168.1.50")
	require.Error(t, err, "a name with a line break was accepted")
	assert.Nil(t, sv)
	assert.Nil(t, key)

	// The seeded local manager row is the only one there should be.
	rows, lerr := store.ListServers(ctx)
	require.NoError(t, lerr)
	for _, r := range rows {
		assert.Equal(t, LocalID, r.ID, "a refused name still created a row: %+v", r)
	}
}
