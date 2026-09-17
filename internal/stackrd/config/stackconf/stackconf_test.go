package stackconf

import (
	"fmt"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const full = `
version: 1
stack: hamrstack
pr_envs: {enabled: true, comment: true}
environments:
  production:
    tiles:
      web:
        type: service
        build: {context: ., dockerfile: Dockerfile}
        port: 8080
        domains:
          - {host: hamrstack.io}
          - {host: www.hamrstack.io, redirect_to: hamrstack.io}
        env:
          PORT: 8080
          APP_ENV: production
          DATABASE_URL: ${secret.db_url}
      db:
        type: managed
        engine: postgres
      worker:
        type: service
        build: {context: ./worker}
  staging:
    tiles:
      web:
        type: service
        build: {context: ., dockerfile: Dockerfile}
        port: 8080
        env:
          PORT: 8080
          APP_ENV: staging
      worker: false
      seed-data:
        type: cron
        image: alpine:3
        schedule: "0 6 * * *"
        command: ./seed.sh
`

// Each env declares its tiles fully; envs resolve independently of each other.
func TestLoadResolve(t *testing.T) {
	r, err := Load([]byte(full), nil)
	require.NoError(t, err)
	assert.Equal(t, "production", r.EnvOrder[0], "default env = %q", r.EnvOrder[0])
	prod, stag := r.Envs["production"], r.Envs["staging"]
	assert.Len(t, prod.Tiles, 3, "production tiles = %d, want 3", len(prod.Tiles))
	// An exclusion entry with no base tile behind it is a no-op.
	_, ok := stag.Tiles["worker"]
	assert.False(t, ok, "staging worker: false entry must be skipped")
	_, ok = stag.Tiles["seed-data"]
	assert.True(t, ok, "staging should have env-local seed-data")
	// Envs are independent: same tile name, different bodies.
	assert.Equal(t, "staging", stag.Tiles["web"].Env["APP_ENV"], "staging web env = %#v", stag.Tiles["web"].Env)
	assert.Equal(t, "8080", stag.Tiles["web"].Env["PORT"], "staging web env = %#v", stag.Tiles["web"].Env)
	assert.Equal(t, "production", prod.Tiles["web"].Env["APP_ENV"], "production web env affected by staging's declaration")
	assert.Equal(t, "hamrstack.io", prod.Tiles["web"].Domains[1].RedirectTo, "redirect domain lost")
	// A managed tile declared under an env is env-scoped.
	assert.Equal(t, "", prod.Tiles["db"].Scope, "env-declared db scope = %q, want unset (env)", prod.Tiles["db"].Scope)
}

// A shared: tile lands in the stack's home with stack scope, whatever the
// order of environments:, that order is the ladder, and reordering it must
// never move a database.
func TestSharedTileLandsInHome(t *testing.T) {
	for _, envs := range []string{"production:\n  staging:\n", "staging:\n  production:\n"} {
		r, err := Load([]byte("version: 1\nstack: s\nshared:\n  pg: {type: managed, engine: postgres}\nenvironments:\n  "+envs), nil)
		require.NoError(t, err)
		pg, ok := r.Envs[repo.HomeSlug].Tiles["pg"]
		require.True(t, ok, "shared pg missing from the home: %#v", r.Envs)
		assert.Equal(t, "stack", pg.Scope)
		assert.Len(t, r.Envs["production"].Tiles, 0, "shared tile copied into production: %#v", r.Envs["production"].Tiles)
		assert.Len(t, r.Envs["staging"].Tiles, 0, "shared tile copied into staging: %#v", r.Envs["staging"].Tiles)
		assert.NotContains(t, r.EnvOrder, repo.HomeSlug, "the home is not a rung")
	}
}

func TestHomeSlugReserved(t *testing.T) {
	_, err := Load([]byte("version: 1\nstack: s\nenvironments: [production, stack]\n"), nil)
	require.ErrorContains(t, err, "reserved")
}

// Swapping the ladder is no change for a shared tile.
func TestDiffSharedTileIgnoresEnvOrder(t *testing.T) {
	state := State{Envs: map[string]EnvState{
		"production": {}, "staging": {},
		repo.HomeSlug: {Tiles: map[string]TileState{
			"pg": {Tile: repo.Tile{Kind: "service", Engine: "postgres", ScopeKind: "stack", ImageRef: managedtiles.Engines["postgres"].DefaultImage}},
		}},
	}}
	for _, envs := range []string{"[production, staging]", "[staging, production]"} {
		r, err := Load([]byte("version: 1\nstack: s\nshared:\n  pg: {type: managed, engine: postgres}\nenvironments: "+envs+"\n"), nil)
		require.NoError(t, err)
		p := Diff(r, state, DiffOpts{})
		assert.Empty(t, p.Changes, "order %s: %+v", envs, p.Changes)
	}
}

func TestSharedRejectsNonManaged(t *testing.T) {
	_, err := Load([]byte("version: 1\nstack: s\nshared:\n  web: {type: service, image: nginx}\nenvironments: [production]\n"), nil)
	require.ErrorContains(t, err, "managed", "shared service tile should be rejected, got %v", err)
}

func TestSharedNameCollisionWithDefaultEnv(t *testing.T) {
	_, err := Load([]byte(`version: 1
stack: s
shared:
  pg: {type: managed, engine: postgres}
environments:
  production:
    tiles:
      pg: {type: managed, engine: postgres}
`), nil)
	require.ErrorContains(t, err, "shared", "shared/default-env name collision should error, got %v", err)
}

// scope: is structural now (shared: vs under an env), not a YAML key.
func TestScopeKeyRejected(t *testing.T) {
	for _, bad := range []string{
		"version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      pg: {type: managed, engine: postgres, scope: stack}\n",
		"version: 1\nstack: s\nshared:\n  pg: {type: managed, engine: postgres, scope: stack}\nenvironments: [production]\n",
	} {
		_, err := Load([]byte(bad), nil)
		assert.Error(t, err, "scope: key should be a strict-decode error for:\n%s", bad)
	}
}

func TestEnvListForm(t *testing.T) {
	r, err := Load([]byte("version: 1\nstack: s\nenvironments: [production, staging]\nshared:\n  pg: {type: managed, engine: postgres}\n"), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"production", "staging"}, r.EnvOrder, "list-form envs broken: %#v", r.EnvOrder)
	assert.Len(t, r.Envs[repo.HomeSlug].Tiles, 1, "list-form shared broken: %#v", r.Envs)
}

func TestDefaults(t *testing.T) {
	r, err := Load([]byte("version: 1\nstack: s\nshared:\n  pg: {type: managed, engine: postgres}\n"), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"production"}, r.EnvOrder, "implicit production missing: %v", r.EnvOrder)
	assert.Equal(t, "stack", r.Envs[repo.HomeSlug].Tiles["pg"].Scope, "shared tile should land in the home")
}

func TestValidation(t *testing.T) {
	env := func(tile string) string {
		return "version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      " + tile + "\n"
	}
	for _, bad := range []string{
		"version: 2\nstack: s\n",         // bad version
		"version: 1\n",                   // no stack
		env("w: {type: service}"),        // service without source
		env("c: {type: cron, image: a}"), // cron without schedule
		env("d: {type: managed, engine: oracle}"),
		env("w: {image: n, bogus_key: 1}"), // unknown key
		"version: 1\nstack: s\npr_envs: {from: nope}\nenvironments:\n  production:\n    tiles:\n      w: {image: n}\n",
		"version: 1\nstack: s\ntiles:\n  w: {image: n}\n", // top-level tiles: is gone
	} {
		_, err := Load([]byte(bad), nil)
		assert.Error(t, err, "expected error for:\n%s", bad)
	}
}

func TestInclude(t *testing.T) {
	main := "version: 1\nstack: s\ninclude: [extra.yml]\nenvironments:\n  production:\n    tiles:\n      web: {image: nginx}\n"
	extra := "environments:\n  production:\n    tiles:\n      worker: {image: busybox}\nshared:\n  pg: {type: managed, engine: postgres}\n"
	r, err := Load([]byte(main), func(path string) ([]byte, error) {
		if path != "extra.yml" {
			return nil, fmt.Errorf("unexpected %s", path)
		}
		return []byte(extra), nil
	})
	require.NoError(t, err)
	assert.Len(t, r.Envs["production"].Tiles, 2, "include not merged: %#v", r.Envs["production"].Tiles)
	assert.Equal(t, "stack", r.Envs[repo.HomeSlug].Tiles["pg"].Scope, "included shared tile lost its stack scope")
}

// base: tiles land in every static env; env entries overlay or exclude.
func TestBaseTiles(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
base:
  tiles:
    web:
      type: service
      image: nginx
      env: {APP_ENV: base, PORT: "80"}
    cache:
      type: service
      image: redis:7
environments:
  production:
  staging:
    tiles:
      cache: false
      web:
        env: {APP_ENV: staging}
      extra: {type: cron, image: alpine, schedule: "0 6 * * *", command: x}
`), nil)
	require.NoError(t, err)
	prod, stag := r.Envs["production"], r.Envs["staging"]
	assert.Len(t, prod.Tiles, 2, "production should get both base tiles: %#v", prod.Tiles)
	_, ok := stag.Tiles["cache"]
	assert.False(t, ok, "staging cache: false must drop the base tile")
	w := stag.Tiles["web"]
	assert.Equal(t, "staging", w.Env["APP_ENV"], "staging web overlay merge wrong: %#v", w)
	assert.Equal(t, "80", w.Env["PORT"], "staging web overlay merge wrong: %#v", w)
	assert.Equal(t, "nginx", w.Image, "staging web overlay merge wrong: %#v", w)
	assert.Equal(t, "base", prod.Tiles["web"].Env["APP_ENV"], "production web must keep the base body untouched")
	_, ok = stag.Tiles["extra"]
	assert.True(t, ok, "env-local tile lost")
}

// A base tile merged in from an include behaves like a locally declared one.
func TestIncludeBaseTiles(t *testing.T) {
	main := "version: 1\nstack: s\ninclude: [extra.yml]\nbase:\n  tiles:\n    web:\n      image: nginx\nenvironments: [production]\n"
	extra := "base:\n  tiles:\n    web:\n      env: {A: b}\n    worker: {image: busybox}\n"
	r, err := Load([]byte(main), func(string) ([]byte, error) { return []byte(extra), nil })
	require.NoError(t, err)
	tiles := r.Envs["production"].Tiles
	assert.Equal(t, "nginx", tiles["web"].Image, "included base overlay wrong: %#v", tiles["web"])
	assert.Equal(t, "b", tiles["web"].Env["A"], "included base overlay wrong: %#v", tiles["web"])
	_, ok := tiles["worker"]
	assert.True(t, ok, "included base tile missing")
}

// pr_envs.tiles overlays base into PRTemplate, which never enters Envs.
func TestPRTemplate(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
secrets:
  SESSION_SECRET: {default: generated}
base:
  tiles:
    web: {image: nginx, port: 80, env: {LOG_LEVEL: info}}
    cache: {image: redis:7}
environments: [production]
pr_envs:
  against: [master]
  tiles:
    cache: false
    web:
      env: {LOG_LEVEL: debug}
`), nil)
	require.NoError(t, err)
	require.NotNil(t, r.PRTemplate, "PRTemplate missing")
	_, ok := r.PRTemplate.Tiles["cache"]
	assert.False(t, ok, "pr exclusion ignored")
	w := r.PRTemplate.Tiles["web"]
	assert.Equal(t, "debug", w.Env["LOG_LEVEL"], "pr overlay merge wrong: %#v", w)
	assert.Equal(t, "nginx", w.Image, "pr overlay merge wrong: %#v", w)
	assert.Equal(t, 80, w.Port, "pr overlay merge wrong: %#v", w)
	_, ok = r.PRTemplate.Secrets["SESSION_SECRET"]
	assert.True(t, ok, "template must carry stack-level secrets")
	assert.Equal(t, []string{"production"}, r.EnvOrder, "template leaked into static envs")
	assert.Equal(t, "info", r.Envs["production"].Tiles["web"].Env["LOG_LEVEL"], "template leaked into static envs")

	// No pr_envs: → no template.
	r2, err := Load([]byte("version: 1\nstack: s\nenvironments: [production]\nshared:\n  pg: {type: managed, engine: postgres}\n"), nil)
	require.NoError(t, err)
	assert.Nil(t, r2.PRTemplate, "PRTemplate should be nil without pr_envs")
}

func TestDiff(t *testing.T) {
	r, err := Load([]byte(full), nil)
	require.NoError(t, err)
	state := State{Envs: map[string]EnvState{
		"production": {Tiles: map[string]TileState{
			"web": {
				Tile: repo.Tile{Kind: "service", SourceType: "git", GitURL: "https://github.com/x/y", GitBranch: "master",
					ContainerPort: 3000, Env: "APP_ENV=production\nPORT=8080\nDATABASE_URL=${secret.db_url}"},
				Domains: []repo.Domain{
					{Host: "hamrstack.io", HTTPS: true},
				},
			},
			"db":     {Tile: repo.Tile{Kind: "service", Engine: "postgres"}},
			"worker": {Tile: repo.Tile{Kind: "service", SourceType: "git", GitURL: "https://github.com/x/y", GitBranch: "master"}},
			"orphan": {Tile: repo.Tile{Kind: "service"}}, // not in config → delete
		}},
		// staging missing entirely → create-env + creates
	}}
	p := Diff(r, state, DiffOpts{GitURL: "https://github.com/x/y", DefaultBranch: "master"})

	find := func(kind, env, tile, field string) *Change {
		for i := range p.Changes {
			c := &p.Changes[i]
			if c.Kind == kind && c.Env == env && c.Tile == tile && strings.HasPrefix(c.Field, field) {
				return c
			}
		}
		return nil
	}
	assert.NotNil(t, find("update", "production", "web", "port"), "missing port update 3000→8080")
	assert.NotNil(t, find("update", "production", "web", "domain +www.hamrstack.io"), "missing redirect domain add")
	assert.NotNil(t, find("delete", "production", "orphan", ""), "strict mode: orphan tile should be deleted")
	assert.NotNil(t, find("create-env", "staging", "", ""), "staging should be created")
	assert.NotNil(t, find("create", "staging", "seed-data", ""), "staging seed-data should be created")
	assert.True(t, p.Destructive(), "plan with deletes must be destructive")
	assert.Contains(t, p.Summary(), "to destroy", "summary: %s", p.Summary())
}

func TestDiffClean(t *testing.T) {
	r, err := Load([]byte("version: 1\nstack: s\nenvironments:\n  production:\n    tiles:\n      web: {image: nginx, port: 80}\n"), nil)
	require.NoError(t, err)
	state := State{Envs: map[string]EnvState{
		"production": {Tiles: map[string]TileState{
			"web": {Tile: repo.Tile{Kind: "service", SourceType: "image", ImageRef: "nginx", ContainerPort: 80}},
		}},
	}}
	p := Diff(r, state, DiffOpts{})
	assert.True(t, p.Empty(), "expected no changes, got %+v", p.Changes)
}

// replicas above 1 on a tile that mounts a volume is a config error, not
// something quietly forced back to 1: a volume is on one machine's disk and
// one container at a time may write it. A config that asks for three and
// silently gets one has been lied to about what is running.
func TestReplicasAgainstVolumes(t *testing.T) {
	err := validateReplicas("web", TileConf{Type: "service", Replicas: 3, Volumes: []string{"data:/var/lib/x"}})
	if err == nil {
		t.Fatal("replicas: 3 on a tile with a volume was accepted")
	}
	if err := validateReplicas("web", TileConf{Type: "service", Replicas: 3}); err != nil {
		t.Fatalf("replicas on a stateless tile refused: %v", err)
	}
	if err := validateReplicas("db", TileConf{Type: "managed", Replicas: 2}); err == nil {
		t.Fatal("replicas on a non-service was accepted")
	}
	if err := validateReplicas("web", TileConf{Type: "service", Replicas: -1}); err == nil {
		t.Fatal("a negative replica count was accepted")
	}
}

// A config file that sets a protect user with no password would lock every
// URL below that level behind a password nobody has. The panel's settings
// forms refuse it; SettingsJSON bypasses them, so Parse has to.
func TestParseRefusesHalfAProtectPair(t *testing.T) {
	const head = "version: 1\nstack: s\n"
	_, err := Parse([]byte(head + "defaults:\n  protect: true\n  protect_user: admin\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "password")

	_, err = Parse([]byte(head + "defaults:\n  protect: true\n  protect_user: admin\n  protect_password: hunter2\n"))
	require.NoError(t, err)

	// Credentials inherited from the org or server level are fine.
	_, err = Parse([]byte(head + "defaults:\n  protect: true\n"))
	require.NoError(t, err)

	_, err = Parse([]byte(head + "environments:\n  prod:\n    defaults:\n      protect_password: hunter2\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "prod")
}
