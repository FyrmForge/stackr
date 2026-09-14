package stackconf

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Presence of from: is what makes an entry a slice; type: is implied.
func TestSliceParse(t *testing.T) {
	conf := func(tile string) string {
		return "version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      site-db: " + tile + "\n"
	}
	r, err := Load([]byte(conf("{from: sharedpg}")), nil)
	require.NoError(t, err)
	tc := r.Envs["production"].Tiles["site-db"]
	assert.Equal(t, "slice", tc.Type, "tile = %+v, want type slice from sharedpg", tc)
	assert.Equal(t, "sharedpg", tc.From, "tile = %+v, want type slice from sharedpg", tc)

	for _, ok := range []string{
		"{from: sharedpg, on_remove: keep}",
		"{from: sharedpg, on_remove: drop}",
		"{from: org.stack.env.pg}", // 4 segments is the widest address
	} {
		_, err := Load([]byte(conf(ok)), nil)
		assert.NoError(t, err, "valid slice rejected: %s", ok)
	}
	for _, bad := range []string{
		"{from: a..b}",                  // empty segment
		"{from: a.b.c.d.e}",             // >4 segments
		"{from: .pg}",                   // leading empty segment
		"{type: slice}",                 // slice without from
		"{from: pg, on_remove: detach}", // detach is a row value, not a file value
	} {
		_, err := Load([]byte(conf(bad)), nil)
		assert.Error(t, err, "expected error for slice %s", bad)
	}
}

// diffOne diffs a single-env config against a single-env state.
func diffOne(tiles map[string]TileConf, state map[string]TileState) *Plan {
	r := &Resolved{Stack: "s", EnvOrder: []string{"production"},
		Envs: map[string]ResolvedEnv{"production": {Tiles: tiles}}}
	s := State{Envs: map[string]EnvState{"production": {Tiles: state}}}
	return Diff(r, s, DiffOpts{})
}

func slState(inst, name, onRemove string) TileState {
	return TileState{Slice: &SliceState{Instance: inst, InstancePath: inst, Name: name, OnRemove: onRemove}}
}

func TestDiffSliceClean(t *testing.T) {
	p := diffOne(
		map[string]TileConf{"site-db": {Type: "slice", From: "sharedpg"}},
		map[string]TileState{"site-db": slState("sharedpg", "site-db", "")})
	require.True(t, p.Empty(), "identical slice produced changes: %+v", p.Changes)
}

func TestDiffSliceOnRemoveUpdate(t *testing.T) {
	p := diffOne(
		map[string]TileConf{"site-db": {Type: "slice", From: "sharedpg", OnRemove: "drop"}},
		map[string]TileState{"site-db": slState("sharedpg", "site-db", "")})
	require.Len(t, p.Changes, 1, "changes = %+v, want exactly one", p.Changes)
	c := p.Changes[0]
	assert.Equal(t, "update", c.Kind, "change = %+v, want update on_remove keep→drop", c)
	assert.Equal(t, "on_remove", c.Field, "change = %+v, want update on_remove keep→drop", c)
	assert.Equal(t, "keep", c.Old, "change = %+v, want update on_remove keep→drop", c)
	assert.Equal(t, "drop", c.New, "change = %+v, want update on_remove keep→drop", c)
}

// Moving a slice to another instance is a replace, and only a held drop
// policy makes the deletion destructive.
func TestDiffSliceInstanceChangeReplaces(t *testing.T) {
	for _, tt := range []struct {
		held     string
		destroys bool
	}{{"drop", true}, {"", false}} {
		p := diffOne(
			map[string]TileConf{"site-db": {Type: "slice", From: "otherpg"}},
			map[string]TileState{"site-db": slState("sharedpg", "site-db", tt.held)})
		require.Len(t, p.Changes, 2, "held=%q: changes = %+v, want delete+create", tt.held, p.Changes)
		require.Equal(t, "delete", p.Changes[0].Kind, "held=%q: changes = %+v, want delete+create", tt.held, p.Changes)
		require.Equal(t, "create", p.Changes[1].Kind, "held=%q: changes = %+v, want delete+create", tt.held, p.Changes)
		assert.Equal(t, tt.destroys, p.Changes[0].Destroys, "held=%q: Destroys = %v, want %v", tt.held, p.Changes[0].Destroys, tt.destroys)
		assert.Contains(t, p.Changes[0].Note, "re-provisions", "held=%q: delete note = %q, want the move warning", tt.held, p.Changes[0].Note)
	}
}

// A config slice landing on a live service tile with the same slug replaces it.
func TestDiffSliceOverLiveTileReplaces(t *testing.T) {
	p := diffOne(
		map[string]TileConf{"site-db": {Type: "slice", From: "sharedpg"}},
		map[string]TileState{"site-db": {Tile: repo.Tile{ID: "t1", Kind: "service"}}})
	require.Len(t, p.Changes, 2, "changes = %+v, want delete tile + create slice", p.Changes)
	require.Equal(t, "delete", p.Changes[0].Kind, "changes = %+v, want delete tile + create slice", p.Changes)
	require.Equal(t, "create", p.Changes[1].Kind, "changes = %+v, want delete tile + create slice", p.Changes)
	require.Equal(t, "slice", p.Changes[1].New, "changes = %+v, want delete tile + create slice", p.Changes)
}

// A live slice absent from config is a strict delete carrying the orphan (or
// destroy) consequence.
func TestDiffSliceUndeclaredStrictDelete(t *testing.T) {
	for _, tt := range []struct {
		held     string
		destroys bool
		note     string
	}{{"", false, "orphaned"}, {"drop", true, "destroyed"}} {
		p := diffOne(nil, map[string]TileState{"site-db": slState("sharedpg", "site-db", tt.held)})
		require.Len(t, p.Changes, 1, "held=%q: changes = %+v, want one delete", tt.held, p.Changes)
		require.Equal(t, "delete", p.Changes[0].Kind, "held=%q: changes = %+v, want one delete", tt.held, p.Changes)
		c := p.Changes[0]
		assert.Equal(t, tt.destroys, c.Destroys, "held=%q: change = %+v, want destroys=%v note~%q", tt.held, c, tt.destroys, tt.note)
		assert.Contains(t, c.Note, tt.note, "held=%q: change = %+v, want destroys=%v note~%q", tt.held, c, tt.destroys, tt.note)
	}
}

// sliceToConf omits the defaults: Name when it equals the slug, OnRemove
// unless the held policy is drop, and what it emits diffs clean.
func TestSliceToConf(t *testing.T) {
	tc := sliceToConf("site-db", &SliceState{Instance: "pg", InstancePath: "pg", Name: "site-db"})
	assert.Equal(t, "slice", tc.Type, "default slice = %+v, want no name, no on_remove", tc)
	assert.Equal(t, "pg", tc.From, "default slice = %+v, want no name, no on_remove", tc)
	assert.Equal(t, "", tc.Name, "default slice = %+v, want no name, no on_remove", tc)
	assert.Equal(t, "", tc.OnRemove, "default slice = %+v, want no name, no on_remove", tc)
	tc = sliceToConf("site-db", &SliceState{InstancePath: "pg", Name: "legacy", OnRemove: "drop"})
	assert.Equal(t, "legacy", tc.Name, "custom slice = %+v, want name legacy on_remove drop", tc)
	assert.Equal(t, "drop", tc.OnRemove, "custom slice = %+v, want name legacy on_remove drop", tc)
	// "detach" is the row-side spelling of keep; it must not leak into the file.
	tc = sliceToConf("s", &SliceState{InstancePath: "pg", Name: "s", OnRemove: "detach"})
	assert.Equal(t, "", tc.OnRemove, "detach serialized as %q, want omitted", tc.OnRemove)

	// Round-trip: a serialized slice state diffs clean against itself.
	st := map[string]TileState{"site-db": slState("sharedpg", "site-db", "drop")}
	r := StateToResolved("s", State{Envs: map[string]EnvState{"production": {Tiles: st}}})
	p := Diff(r, State{Envs: map[string]EnvState{"production": {Tiles: st}}}, DiffOpts{})
	require.True(t, p.Empty(), "slice round-trip not clean: %+v", p.Changes)
}

// The file says cart-stg, postgres stored cart_stg: the same slice, not a
// rename. The diff normalises through the engine before comparing.
func TestDiffSliceNameNormalised(t *testing.T) {
	r, err := Load([]byte("version: 1\nstack: s\nshared:\n  pg: {type: managed, engine: postgres}\nenvironments:\n  staging:\n    tiles:\n      cart-db: {from: pg, name: cart-stg}\n"), nil)
	require.NoError(t, err)
	state := State{Envs: map[string]EnvState{
		"staging": {Tiles: map[string]TileState{
			"cart-db": {Slice: &SliceState{Instance: "pg", Engine: "postgres", Name: "cart_stg"}},
		}},
		repo.HomeSlug: {Tiles: map[string]TileState{
			"pg": {Tile: repo.Tile{Kind: "service", Engine: "postgres", ScopeKind: "stack", ImageRef: managedtiles.Engines["postgres"].DefaultImage}},
		}},
	}}
	p := Diff(r, state, DiffOpts{})
	assert.Empty(t, p.Changes, "%+v", p.Changes)
}
