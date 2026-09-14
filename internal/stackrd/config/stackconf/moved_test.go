package stackconf

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Renaming a key used to plan a delete and a create, which for a tile with a
// volume destroys data. A moved: entry says the two keys are one thing.
func TestMovedPlansARenameNotADeleteAndCreate(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
moved:
  - from: tile.site
    to: tile.web
environments:
  prod:
    tiles:
      web: {image: nginx}
`), nil)
	require.NoError(t, err)

	live := State{Envs: map[string]EnvState{
		"prod": {Tiles: map[string]TileState{"site": liveTile("site")}},
	}}
	p := Diff(r, live, DiffOpts{})
	require.Empty(t, p.Errors, "errors = %v", p.Errors)
	var kinds []string
	for _, c := range p.Changes {
		kinds = append(kinds, c.Kind)
	}
	assert.Contains(t, kinds, "move", "changes = %+v, want a move row", p.Changes)
	assert.NotContains(t, kinds, "delete", "a moved key must not plan a delete: %+v", p.Changes)
	assert.NotContains(t, kinds, "create", "a moved key must not plan a create: %+v", p.Changes)
}

// Already applied: the target is live, the source is gone. That is a stale
// marker, not work, and the warning is the nudge to remove it.
func TestMovedIsANoOpOnceApplied(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
moved:
  - from: tile.site
    to: tile.web
environments:
  prod:
    tiles:
      web: {image: nginx}
`), nil)
	require.NoError(t, err)
	live := State{Envs: map[string]EnvState{"prod": {Tiles: map[string]TileState{"web": liveTile("web")}}}}
	p := Diff(r, live, DiffOpts{})
	require.Empty(t, p.Errors, "errors = %v", p.Errors)
	require.Len(t, p.Warnings, 1, "warnings = %v", p.Warnings)
	assert.Contains(t, p.Warnings[0], "already applied")
}

// Both sides live: the marker claims the target is the old source, but a real
// source still exists. Applying it would merge two rows.
func TestMovedRefusesWhenBothExist(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
moved:
  - from: tile.site
    to: tile.web
environments:
  prod:
    tiles:
      web: {image: nginx}
      site: {image: nginx}
`), nil)
	require.NoError(t, err)
	live := State{Envs: map[string]EnvState{
		"prod": {Tiles: map[string]TileState{"web": liveTile("web"), "site": liveTile("site")}},
	}}
	p := Diff(r, live, DiffOpts{})
	require.Len(t, p.Errors, 1, "errors = %v", p.Errors)
	assert.Contains(t, p.Errors[0], "cannot say which is which")
}

// Every validation is a load error, so a broken marker never reaches a plan.
func TestMovedValidation(t *testing.T) {
	for name, body := range map[string]string{
		"unknown kind":   "  - from: widget.a\n    to: widget.b\n",
		"no kind prefix": "  - from: a\n    to: b\n",
		"kind change":    "  - from: tile.a\n    to: env.b\n",
		"same name":      "  - from: tile.a\n    to: tile.a\n",
		"duplicate from": "  - from: tile.a\n    to: tile.b\n  - from: tile.a\n    to: tile.c\n",
		"duplicate to":   "  - from: tile.a\n    to: tile.c\n  - from: tile.b\n    to: tile.c\n",
		"chained in one": "  - from: tile.a\n    to: tile.b\n  - from: tile.b\n    to: tile.c\n",
	} {
		_, err := Load([]byte("version: 1\nstack: s\nmoved:\n"+body+"environments: [prod]\n"), nil)
		assert.Error(t, err, "%s was accepted", name)
	}
}

// A move whose target the file does not declare is a rename and then a delete.
// Make the author say which one they mean.
func TestMovedTargetMustBeDeclared(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
moved:
  - from: tile.site
    to: tile.web
environments:
  prod:
    tiles:
      other: {image: nginx}
`), nil)
	require.NoError(t, err)
	live := State{Envs: map[string]EnvState{"prod": {Tiles: map[string]TileState{"site": liveTile("site")}}}}
	p := Diff(r, live, DiffOpts{})
	require.Len(t, p.Errors, 1, "errors = %v", p.Errors)
	assert.Contains(t, p.Errors[0], "not declared")
}

// liveTile is a deployed nginx service under one slug: enough for the differ
// to see the same tile before and after a rename.
func liveTile(slug string) TileState {
	return TileState{Tile: repo.Tile{ID: slug + "-id", Slug: slug, Name: slug,
		Kind: "service", SourceType: "image", ImageRef: "nginx"}}
}

// A tile move used to be applied in every environment whatever the plan's
// scope, and planned across every environment too, so an env-branch plan that
// had already moved the row in its own env then read "both exist" for the
// others and the stack was stuck with an erroring plan.
func TestMovedStaysInsideThePlansScope(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
moved:
  - from: tile.site
    to: tile.web
environments:
  prod:
    tiles:
      web: {image: nginx}
  staging:
    tiles:
      web: {image: nginx}
`), nil)
	require.NoError(t, err)

	// staging is already moved, prod is not: stack-wide matching sees both a
	// "site" and a "web" and calls it ambiguous.
	live := State{Envs: map[string]EnvState{
		"prod":    {Tiles: map[string]TileState{"site": liveTile("site")}},
		"staging": {Tiles: map[string]TileState{"web": liveTile("web")}},
	}}

	wide := Diff(r, live, DiffOpts{})
	require.NotEmpty(t, wide.Errors, "a partial move should be ambiguous stack-wide")

	scoped := Diff(r, live, DiffOpts{OnlyEnv: "prod"})
	require.Empty(t, scoped.Errors, "errors = %v", scoped.Errors)
	var moves int
	for _, c := range scoped.Changes {
		if c.Kind == "move" {
			moves++
		}
	}
	assert.Equal(t, 1, moves, "changes = %+v, want the move in scope only", scoped.Changes)
}
