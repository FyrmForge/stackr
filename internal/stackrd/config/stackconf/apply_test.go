package stackconf

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyDiffRoundTrip: applyTileConf is the write-mirror of diffTile, a
// tile built from a config must diff clean against that config.
func TestApplyDiffRoundTrip(t *testing.T) {
	stack := &repo.Stack{ConfigConnectorID: "cn1", ConfigRepo: "org/repo"}
	// Opts() sets Connector = stack.ConfigConnectorID; mirror that here.
	opts := DiffOpts{GitURL: "https://github.com/org/repo", Connector: "cn1", DefaultBranch: "master"}
	cases := map[string]TileConf{
		"service-git": {
			Type: "service", Build: &BuildConf{Context: "apps/web", Dockerfile: "Dockerfile.web"},
			Port: 8080, Healthcheck: "curl -f localhost", SecurityHeaders: true,
			HealthInterval: 10, HealthTimeout: 5, HealthRetries: 5, HealthStartPeriod: 20,
			Volumes: []string{"data:/data"},
			Files:   FileList{"config/loki.yml:/etc/loki/config.yml:template"},
			Storage: []string{"nas-media/tv:/tv:ro"},
			Limits:  &LimitsConf{CPU: 0.5, MemoryMB: 256},
			Env:     EnvMap{"PORT": "8080", "MODE": "canary"},
		},
		"service-image": {Type: "service", Image: "nginx:1.27", Port: 80,
			Command: "-config.file=/etc/x.yml", User: "1000:1000", ShmSizeMB: 128,
			Privileged: true, Devices: []string{"/dev/kmsg"}, Restart: "always",
		},
		"cron": {
			Type: "cron", Image: "alpine:3", Schedule: "0 3 * * *",
			Command: "sh /x.sh", TimeoutMinutes: 15,
		},
		"function": {
			Type: "function", Image: "alpine:3",
			Command: "sh /bootstrap.sh", RunOnDeploy: true, TimeoutMinutes: 10,
			DependsOn: []string{"service-image:started"},
		},
		"db": {Type: "managed", Engine: "postgres"},
		"db-pinned": {Type: "managed", Engine: "postgres",
			Image: "ghcr.io/immich-app/postgres:16", ShmSizeMB: 128},
	}
	for name, tc := range cases {
		tile := repo.Tile{Slug: name, Kind: desiredKind(tc.Type), SourceType: "image"}
		if tc.Type == "managed" {
			tile.Engine = tc.Engine // NewDB needs docker-side defaults; engine is what diff checks
		}
		applyTileConf(&tile, tc, stack, opts)
		p := &Plan{}
		p.diffTile("production", name, tc, TileState{Tile: tile}, EnvState{}, opts)
		assert.Empty(t, p.Changes, "%s: applied tile still diffs: %+v", name, p.Changes)
	}
}

// Nothing applies unattended unless someone asked for it: auto is opt in per
// env, and the file's protected flag overrides even that.
func TestPolicyFor(t *testing.T) {
	auto := &repo.Environment{ApplyPolicy: "auto"}
	manual := &repo.Environment{ApplyPolicy: "manual"}
	unset := &repo.Environment{}
	assert.Equal(t, "auto", PolicyFor(auto, false), "explicit auto")
	assert.Equal(t, "manual", PolicyFor(manual, false), "explicit manual")
	assert.Equal(t, "manual", PolicyFor(unset, false), "unset is manual, no env gets auto for free")
	assert.Equal(t, "manual", PolicyFor(nil, false), "an env with no row is manual")
	assert.Equal(t, "manual", PolicyFor(auto, true), "protected ratchets auto back to manual")
}

// TestDiffScoping: OnlyEnv restricts to one env; SkipEnvs excludes
// branch-bound envs from stack plans (including their strict deletes).
func TestDiffScoping(t *testing.T) {
	r := &Resolved{
		EnvOrder: []string{"production", "staging"},
		Envs: map[string]ResolvedEnv{
			"production": {Tiles: map[string]TileConf{"web": {Type: "service", Image: "a:1", Port: 80}}},
			"staging":    {Tiles: map[string]TileConf{"web": {Type: "service", Image: "a:2", Port: 80}}},
		},
	}
	s := State{Envs: map[string]EnvState{
		"production": {Tiles: map[string]TileState{}},
		"staging":    {Tiles: map[string]TileState{}},
		"legacy":     {Tiles: map[string]TileState{}},
	}}

	p := Diff(r, s, DiffOpts{OnlyEnv: "staging"})
	for _, c := range p.Changes {
		assert.Equal(t, "staging", c.Env, "OnlyEnv leaked change for env %s: %+v", c.Env, c)
	}
	assert.False(t, p.Destructive(), "OnlyEnv plan must not delete other envs")

	p = Diff(r, s, DiffOpts{SkipEnvs: map[string]bool{"staging": true}})
	sawStaging := false
	sawLegacyDelete := false
	for _, c := range p.Changes {
		if c.Env == "staging" {
			sawStaging = true
		}
		if c.Kind == "delete-env" && c.Env == "legacy" {
			sawLegacyDelete = true
		}
	}
	assert.False(t, sawStaging, "SkipEnvs env still planned")
	assert.True(t, sawLegacyDelete, "undeclared env should still be a stack-plan delete")
}

// A single apply creates tiles that depend on each other: a service's uses:
// resolves a db instance by address, so the instance must already exist, and a
// volume's attach target must exist before it. The slugs arrive from a map, so
// without an explicit order this passed or failed by luck.
func TestApplyOrderPutsDBsFirstAndVolumesLast(t *testing.T) {
	re := ResolvedEnv{Tiles: map[string]TileConf{
		"web":      {Type: "service"},
		"sharedpg": {Type: "managed"},
		"uploads":  {Type: "volume"},
		"nightly":  {Type: "cron"},
		"api":      {Type: "service"},
	}}
	// "gone" is a strict-mode delete: no TileConf, so it takes the middle pass.
	got := applyOrder([]string{"uploads", "web", "gone", "nightly", "sharedpg", "api"}, re)
	want := []string{"sharedpg", "api", "gone", "nightly", "web", "uploads"}
	require.Len(t, got, len(want), "applyOrder returned %d slugs, want %d: %v", len(got), len(want), got)
	require.Equal(t, want, got, "applyOrder = %v, want %v", got, want)
}

// An apply is not atomic, it creates containers and databases, which no
// transaction rolls back, so one tile's failure must not abandon the others.
// Aborting early just leaves a different partial state and hides every problem
// but the first, which is three retries to learn three things.
func TestExecuteReportsEveryFailedTile(t *testing.T) {
	changes := map[string]map[string]*tileChange{
		"production": {
			"web": {create: true},
			"api": {fields: map[string]bool{"image": true}},
		},
		"staging": {"worker": {create: true}},
	}
	got := changedTiles(changes)
	require.Equal(t, 3, got, "changedTiles = %d, want 3", got)
}

// Applying a rename moves the row (containers are stopped/redeployed when the
// runtime services are wired; with none wired it is pure DB), the webhook
// auto path refuses it like a destructive change, and the next plan is clean.
func TestApplyStackRename(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")
	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Renamed Stack
environments:
  prod:
    tiles:
      app: {image: nginx}
`)}}
	a := Applier{Planner: pl}

	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	require.Equal(t, "pending", cp.Status)

	ok, err := a.ApplyPlan(ctx, seed.Stack, cp, false)
	require.NoError(t, err, "auto apply")
	assert.False(t, ok, "a webhook push auto-applied a rename")

	ok, err = a.ApplyPlan(ctx, seed.Stack, cp, true)
	require.NoError(t, err, "forced apply")
	require.True(t, ok, "forced apply did not run")
	st, err := store.GetStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed-stack", st.Slug)
	assert.Equal(t, "Renamed Stack", st.Name)

	cp, err = runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "re-plan")
	assert.Equal(t, "clean", cp.Status, "post-rename plan not clean: %s", cp.Plan)
}

// Promote is about a rung, not a plan: it resolves the environment from the
// ladder in the database, so it still works when the config file at the commit
// does not load. The first environment builds on push and is not promoted to.
func TestPromoteNeedsAnUpperRung(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	staging := &repo.Environment{ID: "env2", StackID: seed.Stack.ID, Name: "Staging", Slug: "staging",
		Type: "static", CreatedAt: time.Now().UTC().Add(time.Second)}
	require.NoError(t, s.CreateEnvironment(ctx, staging))

	a := Applier{Planner: Planner{Store: s}}
	assert.Error(t, a.Promote(ctx, seed.Stack, seed.Env.Slug, "abc1234"), "the first rung builds on push")
	assert.Error(t, a.Promote(ctx, seed.Stack, "nope", "abc1234"))
	assert.Error(t, a.Promote(ctx, seed.Stack, "staging", ""), "a promote is always of a commit")
	assert.NoError(t, a.Promote(ctx, seed.Stack, "staging", "abc1234"))
}

// A push that changes both the config and the code drives two paths: the
// apply deploys the tiles the file touched, the branch build covers the rest.
// Applier.Deployed is what the webhook reads to skip the tiles it already
// queued, so an apply must fill it in.
func TestApplyRecordsDeployedTiles(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")
	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Stack
environments:
  prod:
    tiles:
      app: {image: nginx:1.27}
`)}}
	// The unattended path applies nothing unless the env opted in.
	seed.Env.ApplyPolicy = "auto"
	require.NoError(t, store.UpdateEnvironment(ctx, seed.Env), "opt the env into auto")

	deployed := map[string]bool{}
	a := Applier{Planner: pl, Deployed: deployed}

	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	require.Equal(t, "pending", cp.Status)

	ok, err := a.ApplyPlan(ctx, seed.Stack, cp, false)
	require.NoError(t, err, "apply")
	require.True(t, ok, "apply did not run")
	assert.True(t, deployed[seed.Tile.ID], "the applied tile is missing from Deployed: %v", deployed)
}

// The other half of Deployed: a tile whose enqueue failed must come back out
// of the set. Both readers, promoteRest and the push webhook's autoDeploy,
// treat what it holds as "already queued" and skip it, so leaving a failure in
// there turns a push into a build of nothing, logged only as a warning.
func TestDeployDropsTileWhenEnqueueFails(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	// A volume tile is the cheapest guaranteed Enqueue error: it refuses
	// before touching the store or the queue.
	eng := deploy.NewEngine(store, nil, nil, nil, t.TempDir(), nil)
	deployed := map[string]bool{}
	a := Applier{Planner: Planner{Store: store}, Engine: eng, deployed: deployed}

	a.deploy(ctx, &repo.Tile{ID: "vol1", Kind: "volume"}, "test")
	assert.Empty(t, deployed, "a failed enqueue stayed in Deployed: %v", deployed)
}

// Go randomises map iteration. The home env holds the stack-scoped instances
// every other env's slices cut from, so ranging over the change map directly
// created slices before their instance existed on roughly half of all applies:
// "slice cart-db: from \"sharedpg\": no such instance visible from
// stackr-test/staging", from a plan that declared sharedpg in the same apply.
func TestApplyEnvOrderPutsTheHomeFirst(t *testing.T) {
	changes := map[string]map[string]*tileChange{
		"production":  {},
		"staging":     {},
		repo.HomeSlug: {},
		"qa":          {}, // real env, not on the file's ladder
	}
	ladder := []string{"staging", "production"}

	// Run it enough times that a map-order fluke would show.
	for i := 0; i < 200; i++ {
		got := applyEnvOrder(changes, ladder)
		require.Equal(t, []string{repo.HomeSlug, "staging", "production", "qa"}, got,
			"the home env has to come first, then the ladder, then the rest")
	}
}

// An env in the ladder with nothing to change must not appear, and an env in
// the map that the file never declared still has to be applied.
func TestApplyEnvOrderCoversExactlyTheChangedEnvs(t *testing.T) {
	changes := map[string]map[string]*tileChange{"production": {}, "orphan": {}}
	got := applyEnvOrder(changes, []string{"staging", "production"})
	require.Equal(t, []string{"production", "orphan"}, got)

	require.Empty(t, applyEnvOrder(map[string]map[string]*tileChange{}, []string{"staging"}))
}

// One plan covers every env now, so an unattended push would otherwise be
// stopped dead by the first env whose policy is manual, which every env is
// until someone turns it on. holdManual keeps the push applying the auto ones
// and hands the rest to a person.
func TestHoldManualKeepsTheAutoEnvsMoving(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	now := time.Now().UTC()
	require.NoError(t, s.CreateEnvironment(ctx, &repo.Environment{ID: "env2", StackID: seed.Stack.ID,
		Name: "Staging", Slug: "staging", Type: "static", CreatedAt: now.Add(time.Second)}))
	require.NoError(t, s.CreateEnvironment(ctx, &repo.Environment{ID: "env3", StackID: seed.Stack.ID,
		Name: "Canary", Slug: "canary", Type: "static", ApplyPolicy: "auto", CreatedAt: now.Add(2 * time.Second)}))

	a := Applier{Planner: Planner{Store: s}}
	r := &Resolved{
		EnvOrder: []string{seed.Env.Slug, "staging", "canary", "locked"},
		Envs: map[string]ResolvedEnv{
			"locked": {Protected: true},
		},
	}
	skip := map[string]bool{}
	assert.True(t, a.holdManual(ctx, seed.Stack, r, skip))
	assert.True(t, skip[seed.Env.Slug], "even the first env waits: nothing gets auto for free")
	assert.True(t, skip["staging"], "an env with no setting waits for a person")
	assert.False(t, skip["canary"], "an explicit auto setting is the only way in")
	assert.True(t, skip["locked"], "protected in the file ratchets to manual")

	assert.False(t, skip[repo.HomeSlug],
		"canary is applying, so the instances its slices cut from have to land with it")

	// Every env auto means nothing is held and the plan can be marked applied.
	allAuto := map[string]bool{}
	assert.False(t, a.holdManual(ctx, seed.Stack, &Resolved{EnvOrder: []string{"canary"}}, allAuto))

	// Nothing declared is applying, so the shared instances wait too: a push
	// nobody approved must not create databases.
	none := map[string]bool{}
	assert.True(t, a.holdManual(ctx, seed.Stack, &Resolved{EnvOrder: []string{"staging"}}, none))
	assert.True(t, none[repo.HomeSlug])
}

// A slice cutting from an instance that is nowhere used to be discovered tile
// by tile, ten tiles into a walk that had already created the other ten. The
// pre-flight resolves every from: before anything is written, so the answer is
// "nothing was applied".
func TestPreflightRefusesSlicesWithNoInstance(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	a := Applier{Planner: Planner{Store: s}}

	tiles := func(from string) map[string]TileConf {
		return map[string]TileConf{"app-db": {From: from}}
	}
	r := &Resolved{Envs: map[string]ResolvedEnv{
		"production": {Tiles: tiles("sharedpg")},
	}}
	p := &Plan{Changes: []Change{{Kind: "create", Env: "production", Tile: "app-db"}}}

	err := a.preflightSlices(ctx, seed.Stack, r, p)
	require.Error(t, err, "an instance that is nowhere must refuse the apply")
	assert.Contains(t, err.Error(), "nothing was applied")
	assert.Contains(t, err.Error(), "production/app-db")
	assert.Contains(t, err.Error(), "sharedpg")

	// The same plan creating the instance it cuts from is fine: the home env
	// lands first in the walk (applyEnvOrder).
	r.Envs[repo.HomeSlug] = ResolvedEnv{Tiles: map[string]TileConf{"sharedpg": {}}}
	p.Changes = append(p.Changes, Change{Kind: "create", Env: repo.HomeSlug, Tile: "sharedpg"})
	assert.NoError(t, a.preflightSlices(ctx, seed.Stack, r, p), "the instance is created by this very plan")

	// A tile with no from: is not a slice and is never resolved, so a plan
	// full of ordinary services costs nothing here.
	plain := &Resolved{Envs: map[string]ResolvedEnv{
		"production": {Tiles: map[string]TileConf{"site": {}}},
	}}
	assert.NoError(t, a.preflightSlices(ctx, seed.Stack, plain,
		&Plan{Changes: []Change{{Kind: "create", Env: "production", Tile: "site"}}}))
}

// A tile whose deploy failed because a resource was not there yet has to be
// retried when it lands. Rebinding writes the reference into the tile's
// variables but restarts nothing, so without this it stays broken forever:
// seen on the rig as staging/cart-api stuck on "no tile or resource named
// cart-db" long after cart-db existed.
//
// Narrow on purpose. A genuinely broken build must not be redeployed every
// time an apply lands a slice somewhere else in the same environment.
func TestRefBrokenOnlyMatchesAnUnresolvedReference(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	a := Applier{Planner: Planner{Store: store}, deployed: map[string]bool{}}

	last := func(status, errMsg string) {
		require.NoError(t, store.CreateDeployment(ctx, &repo.Deployment{
			ID: uuid.NewString(), TileID: seed.Tile.ID, Status: status, Error: errMsg,
			Trigger: "config", CreatedAt: time.Now().UTC(),
		}))
	}

	assert.False(t, a.refBroken(ctx, seed.Tile), "a tile that has never deployed is not broken on a reference")

	last("error", `CART_DATABASE_URL: ${{ tile.cart-db.DATABASE_URL }}: no tile or resource named "cart-db" in this environment`)
	assert.True(t, a.refBroken(ctx, seed.Tile))

	last("error", "build: exit status 1")
	assert.False(t, a.refBroken(ctx, seed.Tile), "a broken build is not retried behind anyone's back")

	last("done", "")
	assert.False(t, a.refBroken(ctx, seed.Tile))

	// A tile this apply already queued must not be queued twice.
	last("error", `no tile or resource named "cart-db"`)
	a.deployed[seed.Tile.ID] = true
	assert.False(t, a.refBroken(ctx, seed.Tile), "the walk already owns this tile")
}

// Placement lives in the service spec, so a changed node_group or replicas on
// a managed instance has to redeploy it. Leaving them out of needsDBRedeploy
// let a group pin apply as a row write and nothing else: the plan read
// "applied" with the database still scaled to zero on the node it was meant
// to have left.
func TestNeedsDBRedeployCoversPlacementNotJustLimits(t *testing.T) {
	for _, f := range []string{"external_port", "cpu_limit", "memory_mb", "env", "image", "shm_size_mb", "node_group", "replicas"} {
		if !needsDBRedeploy(map[string]bool{f: true}) {
			t.Errorf("%s does not redeploy the instance, but it is in the service spec", f)
		}
	}
	// Scope governs who may provision and no container knows about it.
	if needsDBRedeploy(map[string]bool{"scope": true}) {
		t.Error("scope redeploys the instance, but nothing in the spec changed")
	}
}

// A stored plan is re-diffed at apply time, and that rebuild used to skip the
// var and backup planners that plan time runs. A commit touching only vars:
// therefore diffed to nothing, plan.Empty() short-circuited, the row was
// recorded as "applied", and the values were never written: the plan page
// showed rows nothing would ever act on.
func TestApplyPlanWritesAVarsOnlyCommit(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")

	const tiles = `version: 1
stack: Stack
environments:
  prod:
    tiles:
      app: {image: nginx:1.27}
`
	// Converge first, so the second plan has nothing but the var in it: the
	// bug is only reachable once the tile diff is clean.
	first := Planner{Store: store, Src: staticSrc{file: []byte(tiles)}}
	cp, err := runStack(ctx, first, seed.Stack)
	require.NoError(t, err, "first Run")
	_, err = (Applier{Planner: first, Deployed: map[string]bool{}}).ApplyPlan(ctx, seed.Stack, cp, true)
	require.NoError(t, err, "first apply")

	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Stack
vars:
  FOO: bar
environments:
  prod:
    tiles:
      app: {image: nginx:1.27}
`)}}
	cp, err = runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")

	ok, err := (Applier{Planner: pl, Deployed: map[string]bool{}}).ApplyPlan(ctx, seed.Stack, cp, true)
	require.NoError(t, err, "apply")
	require.True(t, ok, "apply did not run")

	vars, err := store.ListVariables(ctx, repo.OwnerStack, seed.Stack.ID)
	require.NoError(t, err, "list stack vars")
	got := map[string]string{}
	for _, v := range vars {
		got[v.Name] = v.Value
	}
	assert.Equal(t, "bar", got["FOO"], "the declared var was never written: %+v", got)

	// And nothing mistook the stripped rows for a tile: "vars" is a reserved
	// name, so a tile called that is the failure the strip guards against.
	ts, err := store.ListTilesByEnv(ctx, seed.Env.ID)
	require.NoError(t, err)
	for _, tl := range ts {
		assert.NotEqual(t, "vars", tl.Slug, "execute created a tile out of a vars change")
	}

	after, err := store.GetConfigPlan(ctx, cp.ID)
	require.NoError(t, err)
	assert.Equal(t, "applied", after.Status)
}

// A moved: entry tore the old service down, renamed the row, and stopped
// there, so a declared rename left the tile down until someone deployed it by
// hand, which is the opposite of what the docs promise. A service cannot be
// renamed in place under swarm, so the redeploy is the only way it can come
// back at all.
func TestApplyMovedRedeploysTheRenamedTile(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")
	// Only a running tile comes back: that is what restartTile selects on.
	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "running"), "mark the tile running")

	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Stack
moved:
  - from: tile.app
    to: tile.web
environments:
  prod:
    tiles:
      web: {image: nginx}
`)}}
	deployed := map[string]bool{}
	a := Applier{Planner: pl, Deployed: deployed}

	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")

	ok, err := a.ApplyPlan(ctx, seed.Stack, cp, false)
	require.NoError(t, err, "auto apply")
	assert.False(t, ok, "a webhook push auto-applied a moved: entry")

	ok, err = a.ApplyPlan(ctx, seed.Stack, cp, true)
	require.NoError(t, err, "forced apply")
	require.True(t, ok, "forced apply did not run")

	got, err := store.GetTile(ctx, seed.Tile.ID)
	require.NoError(t, err)
	assert.Equal(t, "web", got.Slug, "the row kept its old slug")
	assert.True(t, deployed[seed.Tile.ID],
		"the renamed tile was never redeployed, so it is down under a name nothing resolves: %v", deployed)
}

// RenameStack is exported for the API's PATCH /stacks/{id}, which used to
// write the new slug and nothing else. Service names derive from the slug, so
// every running tile kept serving under the old name while the panel resolved
// the new one, and the runtime swallows a stop against a service that is not
// there. The contract the API needs: the row moves, the old services are torn
// down, and what was running is redeployed.
func TestRenameStackStopsAndRedeploysWhatWasRunning(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	require.NoError(t, store.UpdateTileStatus(ctx, seed.Tile.ID, "running"), "mark the tile running")

	idle := &repo.Tile{ID: "tile-idle", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "worker", Slug: "worker", Kind: "service", SourceType: "image", Status: "idle",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateTile(ctx, idle), "seed an idle tile")

	deployed := map[string]bool{}
	a := Applier{Planner: Planner{Store: store}, Deployed: deployed, deployed: deployed}

	require.NoError(t, a.RenameStack(ctx, seed.Stack, "New Name", &Plan{}), "rename")

	st, err := store.GetStack(ctx, seed.Stack.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-name", st.Slug, "the slug did not move with the name")
	assert.Equal(t, "New Name", st.Name)

	assert.True(t, deployed[seed.Tile.ID],
		"the running tile was not redeployed under the new name: %v", deployed)
	assert.False(t, deployed[idle.ID], "an idle tile was started by a rename: %v", deployed)
}
