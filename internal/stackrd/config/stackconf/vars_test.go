package stackconf

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const varsFile = `version: 1
stack: s
vars:
  LOG_LEVEL: info
  REGION: eu-west
environments:
  prod:
    tiles:
      w: {image: nginx}
  staging:
    vars:
      LOG_LEVEL: debug
    tiles:
      w: {image: nginx}
`

// vars: at stack level lands on the stack owner, an env's own vars: on that
// env's owner, and the env row is the only one written for that env: the
// resolver does the shadowing, so copying the stack value down would freeze it.
func TestApplyVarsWritesBothLevels(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true) // seeds env "prod"

	r, err := Load([]byte(varsFile), nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"LOG_LEVEL": "info", "REGION": "eu-west"}, r.Vars)
	assert.Equal(t, map[string]string{"LOG_LEVEL": "debug"}, r.Envs["staging"].Vars)
	assert.Empty(t, r.Envs["prod"].Vars, "prod declares none; it must not inherit a copy")

	a := applier(Planner{Store: store})
	require.NoError(t, a.applyVars(ctx, seed.Stack, r, "", nil))

	vars, err := store.ListVariables(ctx, repo.OwnerStack, seed.Stack.ID)
	require.NoError(t, err)
	got := map[string]string{}
	for _, v := range vars {
		got[v.Name] = v.Value
		assert.False(t, v.Secret, "vars: must never write a secret row: %+v", v)
	}
	assert.Equal(t, map[string]string{"LOG_LEVEL": "info", "REGION": "eu-west"}, got)

	envVars, err := store.ListVariables(ctx, repo.OwnerEnv, seed.Env.ID)
	require.NoError(t, err)
	assert.Empty(t, envVars, "prod got an env row it never declared: %v", envVars)
}

// A name already stored as a secret is a credential. The file claiming it as a
// plain var errors instead of overwriting, and the apply leaves it alone.
func TestVarsRefuseToOverwriteASecret(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)
	require.NoError(t, store.UpsertVariable(ctx, &repo.Variable{
		OwnerKind: repo.OwnerStack, OwnerID: seed.Stack.ID,
		Name: "REGION", Value: "kept", Secret: true}))

	r, err := Load([]byte(varsFile), nil)
	require.NoError(t, err)
	pl := Planner{Store: store}
	p := &Plan{}
	pl.planVars(ctx, seed.Stack, r, p, "", nil)
	require.Len(t, p.Errors, 1, "errors = %v, want the secret refusal", p.Errors)
	assert.Contains(t, p.Errors[0], "REGION")

	require.NoError(t, applier(pl).applyVars(ctx, seed.Stack, r, "", nil))
	vars, _ := store.ListVariables(ctx, repo.OwnerStack, seed.Stack.ID)
	for _, v := range vars {
		if v.Name == "REGION" {
			assert.Equal(t, "kept", v.Value, "apply overwrote a secret with a file value")
			assert.True(t, v.Secret)
		}
	}
}

// One name cannot be both namespaces: a reference names one or the other, so
// the file would be contradicting itself.
func TestVarsAndSecretsCannotShareAName(t *testing.T) {
	_, err := Load([]byte("version: 1\nstack: s\nsecrets:\n  X:\nvars:\n  X: hi\nenvironments: [prod]\n"), nil)
	require.ErrorContains(t, err, "both")

	_, err = Load([]byte("version: 1\nstack: s\nvars:\n  \"not a var\": hi\nenvironments: [prod]\n"), nil)
	require.ErrorContains(t, err, "not a valid environment variable name")
}

// The bucket slugs sit where a source slug goes, so a tile called one of them
// would make ${{ stack.vars.X }} ambiguous.
func TestReservedTileNames(t *testing.T) {
	for _, name := range []string{"vars", "secrets", "backups"} {
		_, err := Load([]byte("version: 1\nstack: s\nenvironments:\n  prod:\n    tiles:\n      "+
			name+": {image: nginx}\n"), nil)
		require.ErrorContains(t, err, "reserved", "tile named %q was accepted", name)
	}
}
