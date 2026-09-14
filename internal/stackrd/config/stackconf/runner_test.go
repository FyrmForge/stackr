package stackconf

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticSrc serves one config file for every ref; enough to drive a plan
// sweep, which is about which scopes get planned, not about what git returns.
type staticSrc struct{ file []byte }

func (s staticSrc) FileContents(_ context.Context, _ *repo.Connector, _, _, _ string) ([]byte, error) {
	return s.file, nil
}
func (s staticSrc) DefaultBranch(_ context.Context, _ *repo.Connector, _ string) (string, error) {
	return "main", nil
}
func (s staticSrc) HeadSHA(_ context.Context, _ *repo.Connector, _, ref string) (string, error) {
	return "sha-" + ref, nil
}

const sweepConfig = `version: 1
stack: stack
environments:
  prod:
    tiles:
      web: {image: nginx}
  staging:
    tiles:
      web: {image: nginx}
`

// Snapshot has to fold live slices (managed resources + provision rows) into
// the tile-state map under their reference slug. If it gets that wrong, a
// provisioned slice serializes out of the state and the very next plan
// proposes tearing it down, or re-creating it.
func TestSnapshotResolvesSlices(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()

	inst := &repo.Tile{ID: "pg", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "service", Engine: "postgres",
		SourceType: "image", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, inst), "instance")
	consumer := seed.Tile
	consumer.ImageRef = "nginx:1.27" // the seed leaves it blank, which diffs as a source change
	consumer.Env = "LOG_LEVEL=debug\nPRIMARY_DB=${{ tile.site-db.DATABASE_URL }}"
	require.NoError(t, store.UpdateTile(ctx, consumer), "consumer")
	require.NoError(t, store.CreateProvision(ctx, &repo.Provision{ID: "pr1", InstanceTileID: inst.ID,
		EnvID: seed.Env.ID, DBName: "orders", DBUser: "u", DBPassword: "p",
		Status: "active", OnRemove: "drop", ResourceSlug: "site-db", CreatedAt: now}), "provision")
	require.NoError(t, store.CreateResource(ctx, &repo.ManagedResource{ID: "res1",
		EnvironmentID: seed.Env.ID, ProviderTileID: inst.ID, Name: "orders",
		Slug: "site-db", Kind: "postgres", Status: "active", CreatedAt: now, UpdatedAt: now}), "resource")

	state, err := Planner{Store: store}.Snapshot(ctx, seed.Stack)
	require.NoError(t, err, "snapshot")
	ts := state.Envs[seed.Env.Slug].Tiles["site-db"]
	require.NotNil(t, ts.Slice, "slice missing from state: %+v", state.Envs[seed.Env.Slug].Tiles)
	assert.Equal(t, "sharedpg", ts.Slice.Instance, "slice state = %+v", ts.Slice)
	assert.Equal(t, "orders", ts.Slice.Name, "slice state = %+v", ts.Slice)
	assert.Equal(t, "drop", ts.Slice.OnRemove, "slice state = %+v", ts.Slice)
	assert.Equal(t, "sharedpg", ts.Slice.InstancePath, "instance path = %q, want the bare slug for a same-stack instance", ts.Slice.InstancePath)

	// The promise this all exists for: serialize the live env and it diffs clean.
	plan := Diff(StateToResolved(seed.Stack.Slug, state), state, DiffOpts{})
	require.True(t, plan.Empty(), "a provisioned slice did not round-trip clean: %+v", plan.Changes)
}

// Run plans the stack branch and deliberately skips envs pinned to their own
// branch. RunAll is what "re-plan everything" means, and every plan it makes
// has to come back: a caller that saw only the stack plan would report "no
// changes" while a staging deletion sat queued beside it.
func TestRunAllCoversBranchBoundEnvs(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()

	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: now}), "connector")
	// SeedStack's env is "prod" on the stack branch; staging rides its own.
	staging := &repo.Environment{ID: "env-staging", StackID: seed.Stack.ID, Name: "staging",
		Slug: "staging", Type: "static", ConfigBranch: "staging", CreatedAt: now}
	require.NoError(t, store.CreateEnvironment(ctx, staging), "staging env")

	pl := Planner{Store: store, Src: staticSrc{file: []byte(sweepConfig)}}

	one, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	require.Equal(t, "", one.EnvSlug, "Run produced an env-scoped plan %q, want the stack-scoped one", one.EnvSlug)

	all, err := pl.RunAll(ctx, seed.Stack, "")
	require.NoError(t, err, "RunAll")
	require.Len(t, all, 2, "RunAll returned %d plans, want 2 (stack + staging)", len(all))
	assert.Equal(t, "", all[0].EnvSlug, "first plan env = %q, want the stack-scoped plan first", all[0].EnvSlug)
	assert.Equal(t, "staging", all[1].EnvSlug, "second plan env = %q, want staging", all[1].EnvSlug)
	for _, cp := range all {
		assert.NotEmpty(t, cp.ID, "plan not persisted against the stack: %+v", cp)
		assert.Equal(t, seed.Stack.ID, cp.StackID, "plan not persisted against the stack: %+v", cp)
	}
}

// The file's stack: is desired state (identity rides the binding, like the
// org's org: field): a mismatch plans a rename, a collision with a sibling
// stack is a plan error, and an env-scoped plan never judges the stack's name.
func TestPlanStackRename(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: now}), "connector")
	staging := &repo.Environment{ID: "env-staging", StackID: seed.Stack.ID, Name: "staging",
		Slug: "staging", Type: "static", ConfigBranch: "staging", CreatedAt: now}
	require.NoError(t, store.CreateEnvironment(ctx, staging), "staging env")
	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Renamed Stack
environments:
  prod:
    tiles:
      app: {image: nginx}
  staging:
    tiles:
      app: {image: nginx}
`)}}

	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	var stored Plan
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &stored), "stored plan")
	ren := findRename(&stored)
	require.NotNil(t, ren, "no rename change in %+v", stored.Changes)
	assert.Equal(t, "stack", ren.Old)
	assert.Equal(t, "renamed-stack", ren.New)

	// An env-branch plan reads another branch's file; no say over the name.
	envCP, err := pl.RunEnv(ctx, seed.Stack, staging, "")
	require.NoError(t, err, "RunEnv")
	var envStored Plan
	require.NoError(t, json.Unmarshal([]byte(envCP.Plan), &envStored), "stored env plan")
	assert.Nil(t, findRename(&envStored), "env-scoped plan carries the rename: %+v", envStored.Changes)

	// Renaming onto a sibling stack's slug is a plan error, not a change.
	require.NoError(t, store.CreateStack(ctx, &repo.Stack{ID: "stack2", OrgID: seed.Org.ID,
		Name: "Taken", Slug: "renamed-stack", CreatedAt: now}), "sibling stack")
	cp, err = runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run with collision")
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &stored), "stored plan")
	assert.Nil(t, findRename(&stored), "collision still planned a rename")
	require.NotEmpty(t, stored.Errors, "collision produced no plan error")
	assert.Contains(t, stored.Errors[0], "collides")
}

// An org-declared stack is named by the org file's stacks: key; the
// instantiator names the instance, so its own stack: field planning a
// rename is an error. A stale flag on an org that is no longer
// config-managed must not wedge renames.
func TestPlanStackRenameOrgDeclared(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")
	seed.Stack.OrgDeclared = true
	require.NoError(t, store.UpdateStack(ctx, seed.Stack), "flag stack")
	seed.Org.ConfigConnectorID, seed.Org.ConfigRepo = "conn1", "org/org-cfg"
	require.NoError(t, store.UpdateOrg(ctx, seed.Org), "bind org")

	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: Renamed Stack
environments:
  prod:
    tiles:
      app: {image: nginx}
`)}}
	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	var stored Plan
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &stored), "stored plan")
	assert.Nil(t, findRename(&stored), "org-declared stack still planned a rename")
	require.NotEmpty(t, stored.Errors, "no plan error for the owned name")
	assert.Contains(t, stored.Errors[0], "owns this stack's name")

	// Org unbinds its config repo: the stale flag no longer blocks.
	seed.Org.ConfigConnectorID, seed.Org.ConfigRepo = "", ""
	require.NoError(t, store.UpdateOrg(ctx, seed.Org), "unbind org")
	cp, err = runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run after unbind")
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &stored), "stored plan")
	assert.NotNil(t, findRename(&stored), "unbound org left the rename wedged: %+v", stored.Errors)
}

// A stored plan has to carry the secrets waiting to be generated, because that
// is what keeps it out of "clean", and only a plan that lands "pending" is
// picked up by the webhook's auto-apply, which is where minting happens.
func TestRunCarriesPendingMints(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: time.Now().UTC()}), "connector")
	pl := Planner{Store: store, Src: staticSrc{file: []byte(`version: 1
stack: stack
secrets:
  SESSION_SECRET:
    default: generated
environments:
  prod:
    tiles:
      web: {image: nginx}
`)}}

	cp, err := runStack(ctx, pl, seed.Stack)
	require.NoError(t, err, "Run")
	var stored Plan
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &stored), "stored plan")
	assert.Equal(t, []string{"SESSION_SECRET"}, stored.GenSecrets, "stored plan lost the pending mint: %+v", stored.GenSecrets)
	assert.Equal(t, "pending", cp.Status, "plan status = %q, want pending; a clean plan is never applied, so the secret never mints", cp.Status)
}

// runStack is Run's stack-scoped row, the first one: tests written for the
// single-row shape read that.
func runStack(ctx context.Context, pl Planner, stack *repo.Stack) (*repo.ConfigPlan, error) {
	cps, err := pl.Run(ctx, stack, "")
	if err != nil || len(cps) == 0 {
		return nil, err
	}
	return cps[0], nil
}

// A push plans one row, covering every env the file declares. It used to be
// one row per rung, which made a config plan a promotion rung; it is not
// (docs/plans/34-apply-and-review-fixes.md, decision 1). The older row is
// superseded by the next run, so the newest is always the live one.
func TestRunPlansOneRowForEveryEnv(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	now := time.Now().UTC()
	require.NoError(t, store.CreateConnector(ctx, &repo.Connector{ID: "conn1", OrgID: seed.Org.ID,
		Provider: "github", Name: "cfg", CreatedAt: now}), "connector")
	staging := &repo.Environment{ID: "env-staging", StackID: seed.Stack.ID, Name: "staging",
		Slug: "staging", Type: "static", CreatedAt: now}
	require.NoError(t, store.CreateEnvironment(ctx, staging), "staging env")

	pl := Planner{Store: store, Src: staticSrc{file: []byte(sweepConfig)}}
	rows, err := pl.Run(ctx, seed.Stack, "")
	require.NoError(t, err, "Run")
	require.Len(t, rows, 1, "one commit, one plan")
	assert.Equal(t, "", rows[0].EnvSlug, "the row is stack scoped")

	envs := map[string]bool{}
	for _, c := range planChanges(t, rows[0]) {
		envs[c.Env] = true
	}
	assert.True(t, envs["staging"], "the one row must carry staging's changes too: %v", envs)

	again, err := pl.Run(ctx, seed.Stack, "")
	require.NoError(t, err, "second Run")
	old, err := store.GetConfigPlan(ctx, rows[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "superseded", old.Status, "the older row must be superseded by %s", again[0].Status)
}

func planChanges(t *testing.T, cp *repo.ConfigPlan) []Change {
	t.Helper()
	var p Plan
	require.NoError(t, json.Unmarshal([]byte(cp.Plan), &p))
	require.NotEmpty(t, p.Changes, "row %s has no changes", cp.EnvSlug)
	return p.Changes
}
