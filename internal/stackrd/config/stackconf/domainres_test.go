package stackconf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The file owns the stack's own domain resources; inherited org/instance rows
// are never in State.DomainRes, so the file can never delete them.
func TestDiffDomainRes(t *testing.T) {
	state := State{
		DomainRes: []repo.DomainResource{
			{ID: "1", Level: "stack", Host: "keep.example.com", Declared: true},
			{ID: "2", Level: "stack", Host: "gone.example.com", Declared: true},
			{ID: "3", Level: "stack", Host: "flip.example.com", IncludeEnvOnDefault: true, Declared: true},
		},
		AllDomainRes: []repo.DomainResource{
			{ID: "9", Level: "org", Host: "taken.example.com"},
		},
	}
	p := &Plan{}
	p.diffDomainRes([]DomainResConf{
		{Host: "keep.example.com"},
		{Host: "flip.example.com"},
		{Host: "new.example.com"},
		{Host: "taken.example.com"},
	}, state)

	var creates, deletes, updates []string
	for _, c := range p.Changes {
		assert.Equal(t, "stack", c.Env, "domain changes must not land on an env")
		switch c.Kind {
		case "create":
			creates = append(creates, c.New)
		case "delete":
			deletes = append(deletes, c.Old)
		case "update":
			updates = append(updates, c.New)
		}
	}
	assert.ElementsMatch(t, []string{"new.example.com", "taken.example.com"}, creates, "creates")
	assert.ElementsMatch(t, []string{"gone.example.com"}, deletes, "deletes")
	assert.ElementsMatch(t, []string{"flip.example.com include_env_on_default=false"}, updates, "updates")
	assert.Len(t, p.Errors, 1, "a host another owner holds must be a plan error: %v", p.Errors)
}

// A row the panel created is not the file's to delete: until domains: existed
// there was no way to declare one, so deleting undeclared rows on the first
// apply would drop live hostnames and make every plan destructive enough to
// stop webhook auto-apply.
func TestDiffDomainResLeavesPanelRowsAlone(t *testing.T) {
	state := State{DomainRes: []repo.DomainResource{
		{ID: "1", Level: "stack", Host: "panel.example.com"},                    // clickops
		{ID: "2", Level: "stack", Host: "fromfile.example.com", Declared: true}, // the file's
	}}
	p := &Plan{}
	p.diffDomainRes(nil, state) // a file with no domains: at all

	var deletes []string
	for _, c := range p.Changes {
		if c.Kind == "delete" {
			deletes = append(deletes, c.Old)
		}
	}
	assert.Equal(t, []string{"fromfile.example.com"}, deletes,
		"only the declared row is the file's to delete")
}

// Naming a panel row in the file adopts it, an update change, so the plan
// tells the operator the row just changed hands.
func TestDiffDomainResAdoptsPanelRow(t *testing.T) {
	state := State{DomainRes: []repo.DomainResource{
		{ID: "1", Level: "stack", Host: "panel.example.com"},
	}}
	p := &Plan{}
	p.diffDomainRes([]DomainResConf{{Host: "panel.example.com"}}, state)

	assert.Len(t, p.Changes, 1, "adoption is one change: %v", p.Changes)
	assert.Equal(t, "update", p.Changes[0].Kind)
	assert.Equal(t, "panel.example.com", p.Changes[0].New)
	assert.NotEmpty(t, p.Changes[0].Note, "the plan has to say the file is taking it over")

	// Already declared and matching: nothing to do.
	p2 := &Plan{}
	p2.diffDomainRes([]DomainResConf{{Host: "panel.example.com"}},
		State{DomainRes: []repo.DomainResource{{ID: "1", Host: "panel.example.com", Declared: true}}})
	assert.Empty(t, p2.Changes, "a settled declared row is not a change")
}

func TestValidateDomains(t *testing.T) {
	assert.NoError(t, ValidateDomains([]DomainResConf{{Host: "a.com"}, {Host: "b.com"}}), "distinct hosts")
	assert.Error(t, ValidateDomains([]DomainResConf{{Host: "a.com"}, {Host: "a.com"}}), "duplicate host")
	assert.Error(t, ValidateDomains([]DomainResConf{{Host: "  "}}), "blank host")
}

// A host another tile already routes is a plan error, not a create that dies
// on the unique index at apply. Two tiles in one file claiming the same host
// fail the same way.
func TestDiffTakenHost(t *testing.T) {
	const cfg = `
version: 1
stack: hosts
environments:
  production:
    tiles:
      web:
        type: service
        image: nginx
        domains:
          - host: app.example.com
      api:
        type: service
        image: nginx
        domains:
          - host: api.example.com
          - host: dup.example.com
      admin:
        type: service
        image: nginx
        domains:
          - host: dup.example.com
`
	r, err := Load([]byte(cfg), nil)
	require.NoError(t, err)
	state := State{
		Envs:       map[string]EnvState{"production": {Tiles: map[string]TileState{}}},
		AllDomains: []repo.Domain{{TileID: "other", Host: "app.example.com"}},
	}
	p := Diff(r, state, DiffOpts{})
	assert.Contains(t, p.Errors, "env production: tile web: app.example.com already routes to another service")
	assert.Contains(t, p.Errors, "env production: tile api: dup.example.com already routes to another service")
	for _, e := range p.Errors {
		assert.NotContains(t, e, "api.example.com")
	}

	// The same host on the tile that owns it is not a collision.
	state.AllDomains[0].TileID = "web-id"
	state.Envs["production"].Tiles["web"] = TileState{Tile: repo.Tile{ID: "web-id", Kind: "service"}, Domains: []repo.Domain{{TileID: "web-id", Host: "app.example.com"}}}
	p = Diff(r, state, DiffOpts{})
	for _, e := range p.Errors {
		assert.NotContains(t, e, "app.example.com")
	}
}

// Auto hostnames follow the file's ladder. On a first apply the store only
// knows the env the org file made, so a stale DefaultEnv in opts must lose to
// the file's first env or staging is created with an env label it then drops.
func TestAutoHostUsesFileDefaultEnv(t *testing.T) {
	r, err := Load([]byte("version: 1\nstack: s\nenvironments:\n  staging:\n    tiles:\n      web: {image: nginx, port: 80, domains: [{auto: true}]}\n  production:\n"), nil)
	require.NoError(t, err)
	p := Diff(r, State{Envs: map[string]EnvState{}}, DiffOpts{
		OrgSlug: "o", StackSlug: "s", DefaultEnv: "production",
		DomainResources: []repo.DomainResource{{Host: "example.com", Level: "stack"}},
	})
	assert.Empty(t, p.Errors)
	_, bare := p.hostOwner["web.example.com"]
	assert.True(t, bare, "staging is the file's default env, claimed: %v", p.hostOwner)
}

// A hostname moving between two tiles is one change, not a conflict with
// itself. The plan that takes the bare host off production and gives it to
// staging used to be refused with "already routes to another service", and a
// stack in that state had no way forward inside the product.
func TestDiffLetsAPlanMoveAHostItReleases(t *testing.T) {
	const cfg = `
version: 1
stack: hosts
environments:
  staging:
    tiles:
      web:
        type: service
        image: nginx
        domains:
          - host: app.example.com
  production:
    tiles:
      web:
        type: service
        image: nginx
        domains:
          - host: prod.example.com
`
	r, err := Load([]byte(cfg), nil)
	require.NoError(t, err)

	// production/web holds app.example.com today; the file now gives it to
	// staging and asks production for a different one.
	prodWeb := repo.Tile{ID: "tile-prod-web", Slug: "web"}
	held := repo.Domain{TileID: prodWeb.ID, Host: "app.example.com"}
	state := State{
		Envs: map[string]EnvState{
			"staging": {Tiles: map[string]TileState{}},
			"production": {Tiles: map[string]TileState{
				"web": {Tile: prodWeb, Domains: []repo.Domain{held}},
			}},
		},
		AllDomains: []repo.Domain{held},
	}
	p := Diff(r, state, DiffOpts{})
	for _, e := range p.Errors {
		assert.NotContains(t, e, "already routes to another service",
			"the plan releases that host itself: %v", p.Errors)
	}

	// A row the plan does not touch still blocks. Same host, but held by a
	// tile in an environment this plan skips.
	state.Envs["production"] = EnvState{Tiles: map[string]TileState{
		"web": {Tile: prodWeb, Domains: []repo.Domain{held}},
	}}
	p = Diff(r, state, DiffOpts{SkipEnvs: map[string]bool{"production": true}})
	assert.Contains(t, p.Errors, "env staging: tile web: app.example.com already routes to another service")
}
