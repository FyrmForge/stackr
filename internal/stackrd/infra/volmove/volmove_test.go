package volmove

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A tile that mounts two volumes has to move both. Carrying one and setting
// the home node anyway is silent data loss: the task comes up on the target
// against an empty directory and swarm reports it healthy.
func TestVolumesOfReturnsEveryAttachedVolumeNotTheFirst(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	// Two volume subtiles on the one service tile. Created in an order that
	// does not match their sorted order, so a test that passes by accident
	// because the store happened to return them alphabetically does not.
	for _, id := range []string{"zz111111-vol", "aa222222-vol"} {
		v := &repo.Tile{ID: id, StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
			Name: id, Slug: id, Kind: "volume", AttachedTileID: seed.Tile.ID,
			Status: "idle", CreatedAt: now, UpdatedAt: now}
		if err := store.CreateTile(ctx, v); err != nil {
			t.Fatalf("create volume tile %s: %v", id, err)
		}
	}

	s := &Service{Store: store}
	got, err := s.volumesOf(ctx, seed.Tile)
	if err != nil {
		t.Fatalf("volumesOf: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("moving %d volumes, want both of them: %v", len(got), got)
	}
	// Sorted, so a move copies them in the same order every time.
	if got[0] > got[1] {
		t.Errorf("volumes not sorted: %v", got)
	}
	want := map[string]bool{"stackr-vol-aa222222": false, "stackr-vol-zz111111": false}
	for _, v := range got {
		if _, ok := want[v]; !ok {
			t.Errorf("unexpected volume %q in %v", v, got)
			continue
		}
		want[v] = true
	}
	for v, seen := range want {
		if !seen {
			t.Errorf("%s was left behind on the source node", v)
		}
	}
}

// A volume tile clicked directly is its own answer, and its home node comes
// from the tile it is attached to rather than from its own empty column.
func TestVolumeTileMovesItselfAndTakesItsHomeNodeFromTheTileThatMountsIt(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	if err := store.SetTileHomeNode(ctx, seed.Tile.ID, "node-worker"); err != nil {
		t.Fatalf("set home node: %v", err)
	}
	vol := &repo.Tile{ID: "bb333333-vol", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "data", Slug: "data", Kind: "volume", AttachedTileID: seed.Tile.ID,
		Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateTile(ctx, vol); err != nil {
		t.Fatalf("create volume tile: %v", err)
	}

	s := &Service{Store: store}
	got, err := s.volumesOf(ctx, vol)
	if err != nil {
		t.Fatalf("volumesOf: %v", err)
	}
	if len(got) != 1 || got[0] != "stackr-vol-bb333333" {
		t.Errorf("volumesOf(volume tile) = %v, want its own volume", got)
	}

	// The volume tile's own home_node is empty; reading it directly is what
	// told an operator a 238 MB volume had nothing to move.
	if vol.HomeNode != "" {
		t.Fatalf("test setup wrong: volume tile has a home node of its own")
	}
	if node := s.sourceNode(ctx, vol); node != "node-worker" {
		t.Errorf("sourceNode = %q, want the attached tile's node", node)
	}
}

// An empty target is not "wherever it is". agent.Nodes.IsSelf answers true for
// the empty string, so an unchecked empty target resolves to the manager.
func TestStartRefusesAnEmptyTarget(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	if err := store.SetTileHomeNode(ctx, seed.Tile.ID, "node-worker"); err != nil {
		t.Fatalf("set home node: %v", err)
	}
	vol := &repo.Tile{ID: "cc444444-vol", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "data", Slug: "data", Kind: "volume", AttachedTileID: seed.Tile.ID,
		Status: "idle", CreatedAt: now, UpdatedAt: now}
	if err := store.CreateTile(ctx, vol); err != nil {
		t.Fatalf("create volume tile: %v", err)
	}
	tile, err := store.GetTile(ctx, seed.Tile.ID)
	if err != nil {
		t.Fatalf("get tile: %v", err)
	}

	s := &Service{Store: store, moves: map[string]*Move{}}
	// Refused before anything is reached over the network, so a nil Nodes is
	// proof the guard fired rather than the reachability check.
	if _, err := s.Start(ctx, tile, ""); err == nil {
		t.Fatal("Start accepted an empty target; it resolves to the manager")
	}
}

// The modal counted 0 B for a 6 MB move and still said 0 B next to
// "done". Neither end of the wire reports the finish, so the tally seeds its
// totals from the sizes the modal already measured and counts a finished leg
// whole.
func TestTallyEndsFullEvenWhenTheWireSaysNothing(t *testing.T) {
	const mb = 3 << 20
	tal := newTally(2)
	tal.seed(0, mb)
	tal.seed(1, mb)

	done, total := tal.sum()
	require.Zero(t, done)
	require.EqualValues(t, 2*mb, total, "the denominator is right before a byte moves")

	// rsync reported nothing at all for this leg, which is the case that
	// produced "0 B" on the rig.
	tal.complete(0)
	done, total = tal.sum()
	require.EqualValues(t, mb, done)
	require.EqualValues(t, 2*mb, total)

	tal.report(1, mb/2, mb)
	done, _ = tal.sum()
	require.EqualValues(t, mb+mb/2, done)

	tal.complete(1)
	done, total = tal.sum()
	require.EqualValues(t, total, done, "a finished move reads as complete")
}

// The delta pass copies less than the first pass. Counting it must not make
// the bar go backwards.
func TestTallyNeverGoesBackwards(t *testing.T) {
	tal := newTally(1)
	tal.seed(0, 1000)
	tal.report(0, 1000, 1000)
	tal.complete(0)

	tal.report(0, 8, 1000) // the delta pass: 8 bytes changed
	tal.complete(0)
	done, total := tal.sum()
	require.EqualValues(t, 1000, done)
	require.EqualValues(t, 1000, total)
}

// A zero total on the wire must not wipe the seeded size, which is what the
// agent's closing frame carries.
func TestTallyIgnoresAZeroTotalFromTheWire(t *testing.T) {
	tal := newTally(1)
	tal.seed(0, 500)
	tal.report(0, 0, 0)
	_, total := tal.sum()
	require.EqualValues(t, 500, total)
}

// ETA divides by Bytes and guards on it, so it stayed silent for every move
// while the counters were stuck at zero. With real counters it answers.
func TestETAAnswersOnceTheCountersAreReal(t *testing.T) {
	_, ok := Move{Total: 100, Bytes: 0, Started: time.Now().Add(-time.Second)}.ETA()
	require.False(t, ok, "nothing copied yet, nothing to extrapolate from")

	d, ok := Move{Total: 100, Bytes: 25, Started: time.Now().Add(-10 * time.Second)}.ETA()
	require.True(t, ok)
	require.InDelta(t, 30*time.Second, d, float64(2*time.Second))
}

// A managed instance keeps its data in one docker volume named after the
// tile, with no volume subtile pointing at it. The sibling walk found nothing
// there, so the Move modal told an operator a postgres instance holding
// twelve databases had "no volume to move", on a plan whose own blocking
// message said it holds one.
func TestVolumesOfFindsAManagedInstancesOwnVolume(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "2e0ba04c-d1ec-4f65-aea1-b161799faa3c",
		StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "database", Engine: "postgres",
		Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, inst), "create instance tile")

	s := &Service{Store: store}
	got, err := s.volumesOf(ctx, inst)
	require.NoError(t, err, "volumesOf on a managed instance")
	require.Equal(t, []string{"stackr-db-2e0ba04c"}, got)
}
