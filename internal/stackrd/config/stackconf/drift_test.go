package stackconf

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func changeOf(p *Plan, kind, tile, field string) *Change {
	for i := range p.Changes {
		c := &p.Changes[i]
		if c.Kind == kind && c.Tile == tile && strings.HasPrefix(c.Field, field) {
			return c
		}
	}
	return nil
}

// A db engine change is a replace, not a column edit, writing the column left
// a running postgres labelled mysql. The approver must also see that the data
// is orphaned.
func TestEngineChangeReplaces(t *testing.T) {
	p := &Plan{}
	tile := repo.Tile{Kind: "service", Engine: "postgres"}
	p.diffTile("production", "db", TileConf{Type: "managed", Engine: "mysql"}, TileState{Tile: tile}, EnvState{}, DiffOpts{})

	del, add := changeOf(p, "delete", "db", ""), changeOf(p, "create", "db", "")
	require.NotNil(t, del, "want delete+create, got %+v", p.Changes)
	require.NotNil(t, add, "want delete+create, got %+v", p.Changes)
	assert.Nil(t, changeOf(p, "update", "db", "engine"), "engine must not also appear as an in-place update")
	assert.Contains(t, del.Note, "not migrated", "data-loss note missing from the change the approver sees: %q", del.Note)
}

// A db instance's port and sharing scope used to be invisible to the diff, so
// the file could not express them and a UI edit never showed as drift.
func TestDBPortAndScopeDiff(t *testing.T) {
	p := &Plan{}
	tile := repo.Tile{Kind: "service", Engine: "postgres", ExternalPort: 15432, ScopeKind: "env"}
	p.diffTile("production", "pg", TileConf{Type: "managed", Engine: "postgres", ExternalPort: 25432, Scope: "org"},
		TileState{Tile: tile}, EnvState{}, DiffOpts{})

	port := changeOf(p, "update", "pg", "external_port")
	require.NotNil(t, port, "external_port diff = %+v", p.Changes)
	require.Equal(t, "15432", port.Old, "external_port diff = %+v", p.Changes)
	require.Equal(t, "25432", port.New, "external_port diff = %+v", p.Changes)
	scope := changeOf(p, "update", "pg", "scope")
	require.NotNil(t, scope, "scope diff = %+v", p.Changes)
	require.Equal(t, "env", scope.Old, "scope diff = %+v", p.Changes)
	require.Equal(t, "org", scope.New, "scope diff = %+v", p.Changes)
	assert.Empty(t, scope.Note, "widening scope needs no warning, got %q", scope.Note)
}

// Absent external_port / scope: mean the defaults, not "leave as is", the
// same one-way-ratchet trap the limits: block already avoids.
func TestDBFieldsReturnToDefaults(t *testing.T) {
	p := &Plan{}
	tile := repo.Tile{Kind: "service", Engine: "postgres", ExternalPort: 15432, ScopeKind: "org"}
	p.diffTile("production", "pg", TileConf{Type: "managed", Engine: "postgres"},
		TileState{Tile: tile}, EnvState{}, DiffOpts{})

	port := changeOf(p, "update", "pg", "external_port")
	require.NotNil(t, port, "dropping external_port should diff to 0, got %+v", p.Changes)
	require.Equal(t, "0", port.New, "dropping external_port should diff to 0, got %+v", p.Changes)
	scope := changeOf(p, "update", "pg", "scope")
	require.NotNil(t, scope, "dropping scope should diff to env, got %+v", p.Changes)
	require.Equal(t, "env", scope.New, "dropping scope should diff to env, got %+v", p.Changes)
	// Narrowing strands existing consumers; the approver has to be told.
	assert.Contains(t, scope.Note, "no longer provision", "narrowing note missing from the change the approver sees: %q", scope.Note)
}

// Compose tiles are gone (docs/plans/30-docker-swarm.md): docker stack deploy
// silently drops build, depends_on, container_name, restart, privileged and
// host networking, so a compose file under Swarm is a different file. The
// parser is strict, so the old keys fail loudly instead of being ignored.
func TestComposeKeysRejected(t *testing.T) {
	env := func(tile string) string {
		return "version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      " + tile + "\n"
	}
	for _, gone := range []string{
		env("w: {compose: docker/compose.yml}"),
		env("w: {compose_inline: \"services: {a: {image: x}}\"}"),
	} {
		_, err := Load([]byte(gone), nil)
		assert.Error(t, err, "compose key should be rejected:\n%s", gone)
	}
}

// yaml.v3 says "field compose_inline not found in type stackconf.TileConf".
// That reaches an operator on the config plan page, where the Go type name
// means nothing and the line number counts lines in a re-marshalled fragment
// rather than in the file they wrote. A removed key also has to say what
// replaced it: the fixture repo carried compose_inline for months after the
// schema dropped it, and the message never hinted at image:.
func TestUnknownKeyErrorsReadLikeYAMLNotGo(t *testing.T) {
	cfg := func(tile string) string {
		return "version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      " + tile + "\n"
	}

	_, err := Load([]byte(cfg("w: {compose_inline: \"services: {}\"}")), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown key compose_inline")
	assert.Contains(t, err.Error(), "image:", "a removed key has to name its replacement")
	assert.NotContains(t, err.Error(), "stackconf.", "no Go type names")
	assert.NotContains(t, err.Error(), "line 1", "no line number from a re-marshalled fragment")

	// A key that was never in the schema is a typo, so it gets the plain
	// form with no hint invented for it.
	_, err = Load([]byte(cfg("w: {imag: redis}")), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown key imag")
	assert.NotContains(t, err.Error(), "no longer supported")
	assert.NotContains(t, err.Error(), "stackconf.")

	// Errors that are not about an unknown key must pass through untouched.
	_, err = Load([]byte("version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      w: {port: nope}\n"), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "unknown key")
}

// Removing limits: from the file must clear them. It used to be a one-way
// ratchet, set once, never removable.
func TestLimitsRemovalDiffs(t *testing.T) {
	p := &Plan{}
	tile := repo.Tile{Kind: "service", SourceType: "image", ImageRef: "nginx", CPULimit: 0.5, MemLimitMB: 256}
	p.diffTile("production", "web", TileConf{Type: "service", Image: "nginx"}, TileState{Tile: tile}, EnvState{}, DiffOpts{})

	cpu := changeOf(p, "update", "web", "cpu_limit")
	mem := changeOf(p, "update", "web", "memory_mb")
	require.NotNil(t, cpu, "removing limits: should diff both to 0, got %+v", p.Changes)
	require.Equal(t, "0", cpu.New, "removing limits: should diff both to 0, got %+v", p.Changes)
	require.NotNil(t, mem, "removing limits: should diff both to 0, got %+v", p.Changes)
	require.Equal(t, "0", mem.New, "removing limits: should diff both to 0, got %+v", p.Changes)
}

// Same ratchet on cron timeout: absent means the 30-minute default.
func TestCronTimeoutReturnsToDefault(t *testing.T) {
	p := &Plan{}
	tile := repo.Tile{Kind: "cron", SourceType: "image", ImageRef: "alpine:3", Cron: "0 3 * * *", TimeoutMinutes: 90}
	p.diffTile("production", "job", TileConf{Type: "cron", Image: "alpine:3", Schedule: "0 3 * * *"}, TileState{Tile: tile}, EnvState{}, DiffOpts{})

	c := changeOf(p, "update", "job", "timeout_minutes")
	require.NotNil(t, c, "dropping timeout_minutes should diff back to 30, got %+v", p.Changes)
	require.Equal(t, "30", c.New, "dropping timeout_minutes should diff back to 30, got %+v", p.Changes)
}

// Typos above the tiles: level used to be silently ignored while the same typo
// inside a tile body errored.
func TestUnknownKeysRejected(t *testing.T) {
	for _, bad := range []string{
		"version: 1\nstack: s\nenviroments: [production]\n", //nolint:misspell // the typo is the case under test
		"version: 1\nstack: s\nenvironments:\n  production:\n    bogus: 1\n",
		"version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      w: {image: n, deploy: {on: push}}\n",         // deleted field
		"version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      c: {type: cron, image: a, schedule: nope}\n", // invalid cron, both paths
	} {
		_, err := Load([]byte(bad), nil)
		assert.Error(t, err, "expected a parse error for:\n%s", bad)
	}
}

// A pr_envs block that doesn't mention enabled: must inherit the panel setting.
// A plain bool made `pr_envs: {from: staging}` silently turn PR envs off.
func TestPREnvsPartialBlockInherits(t *testing.T) {
	f, err := Parse([]byte("version: 1\nstack: s\npr_envs: {comment: true}\nenvironments: [production]\n"))
	require.NoError(t, err)
	require.NotNil(t, f.PREnvs, "enabled should be nil (inherit), got %+v", f.PREnvs)
	require.Nil(t, f.PREnvs.Enabled, "enabled should be nil (inherit), got %+v", f.PREnvs)
	off, err := Parse([]byte("version: 1\nstack: s\npr_envs: {enabled: false}\nenvironments: [production]\n"))
	require.NoError(t, err)
	require.NotNil(t, off.PREnvs.Enabled, "explicit enabled: false must still disable")
	assert.False(t, *off.PREnvs.Enabled, "explicit enabled: false must still disable")
}

// Traefik routes off Domain.ContainerPort, not Tile.ContainerPort: a config
// port: change rebuilt the container on the new port while the proxy kept
// routing the old one.
func TestPortChangeReachesDomains(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	ctx := context.Background()

	tile := seed.Tile
	tile.ContainerPort = 3000
	require.NoError(t, store.UpdateTile(ctx, tile))
	tracking := &repo.Domain{ID: "d1", TileID: tile.ID, Host: "app.test", ContainerPort: 3000, HTTPS: true, CreatedAt: time.Now().UTC()}
	pinned := &repo.Domain{ID: "d2", TileID: tile.ID, Host: "api.test", ContainerPort: 9999, HTTPS: true, CreatedAt: time.Now().UTC()}
	for _, d := range []*repo.Domain{tracking, pinned} {
		require.NoError(t, store.CreateDomain(ctx, d))
	}

	a := Applier{Planner: Planner{Store: store}}
	tc := TileConf{Type: "service", Image: "nginx", Port: 8080,
		Domains: []DomainConf{{Host: "app.test"}, {Host: "api.test"}}}
	require.NoError(t, a.syncDomains(ctx, seed.Env, tile, tc, DiffOpts{}, 3000))

	got, err := store.ListDomainsByTile(ctx, tile.ID)
	require.NoError(t, err)
	byHost := map[string]int{}
	for _, d := range got {
		byHost[d.Host] = d.ContainerPort
	}
	assert.Equal(t, 8080, byHost["app.test"], "domain tracking the old port should follow it to 8080, got %d", byHost["app.test"])
	assert.Equal(t, 9999, byHost["api.test"], "deliberately pinned port must be left alone, got %d", byHost["api.test"])
}
