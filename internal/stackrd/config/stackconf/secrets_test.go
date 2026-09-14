package stackconf

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The file declares that a secret must exist; the value is set from the panel
// or the CLI (or generated) and never lives in git. Declarations are a map:
// bare keys keep the set-out-of-band semantics, env lists add names.
func TestSecretsMergeStackAndEnv(t *testing.T) {
	r, err := Load([]byte(`version: 1
stack: s
secrets:
  STRIPE_KEY:
  SMTP_PASSWORD:
  SESSION_SECRET:
    default: generated
    length: 14
    include_numbers: true
    required: true
environments:
  production:
    secrets: [PROD_ONLY, STRIPE_KEY]
  staging:
`), nil)
	require.NoError(t, err)
	prod := r.Envs["production"].Secrets
	for _, name := range []string{"PROD_ONLY", "SMTP_PASSWORD", "STRIPE_KEY", "SESSION_SECRET"} {
		assert.Contains(t, prod, name, "production is missing secret %s: %v", name, prod)
	}
	_, ok := r.Envs["staging"].Secrets["PROD_ONLY"]
	assert.False(t, ok, "staging inherited an env-local declaration")
	sc := prod["SESSION_SECRET"]
	assert.Equal(t, "generated", sc.Default, "options lost in the merge: %+v", sc)
	assert.Equal(t, 14, sc.Length, "options lost in the merge: %+v", sc)
	assert.True(t, sc.IncludeNumbers, "options lost in the merge: %+v", sc)
	assert.True(t, sc.Required, "options lost in the merge: %+v", sc)
	assert.True(t, sc.PerEnv(), "options lost in the merge: %+v", sc)
}

// A name that could never be an environment variable can never be satisfied,
// so it would warn forever. And the old list form died with the format break.
func TestSecretsValidation(t *testing.T) {
	for _, bad := range []string{
		"version: 1\nstack: s\nsecrets:\n  \"not a var\":\nenvironments: [production]\n",
		"version: 1\nstack: s\nsecrets: [STRIPE_KEY]\nenvironments: [production]\n",
		"version: 1\nstack: s\nsecrets:\n  X:\n    default: banana\nenvironments: [production]\n",
		"version: 1\nstack: s\nsecrets:\n  X:\n    length: 9\nenvironments: [production]\n",
	} {
		_, err := Load([]byte(bad), nil)
		assert.Error(t, err, "expected a validation error for:\n%s", bad)
	}
}

// Missing secrets warn; required ones included, because "required" says the
// stack needs the value to run, not that it may not be created; they come back
// as inputs the plan page collects. Generated ones announce themselves, and an
// env override on env_versions: false is refused loudly.
func TestSecretIssues(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	r, err := Load([]byte(`version: 1
stack: s
secrets:
  STRIPE_KEY:
  SMTP_PASSWORD:
    required: true
  SESSION_SECRET:
    default: generated
  PINNED:
    env_versions: false
environments:
  prod:
    tiles:
      w: {image: nginx}
`), nil)
	require.NoError(t, err)
	pl := Planner{Store: store}

	errs, warns, gen := pl.SecretIssues(ctx, seed.Stack, r, "", nil)
	assert.Equal(t, []string{"SESSION_SECRET"}, gen, "gen = %v, want the generated secret pending a mint", gen)
	assert.Empty(t, errs, "errs = %v, nothing unset may block an apply", errs)
	// Unset and generated secrets are plan rows, not banners.
	assert.Empty(t, warns, "warns = %v, want no banner for a declared secret", warns)

	// The three unset ones are the boxes the plan page offers; the generated
	// one is not, the apply mints it.
	inputs := pl.DeclaredInputs(ctx, seed.Stack, r, "", nil)
	var names []string
	for _, in := range inputs {
		names = append(names, in.Name)
	}
	assert.Equal(t, []string{"PINNED", "SMTP_PASSWORD", "STRIPE_KEY"}, names, "inputs = %v", names)
	for _, in := range inputs {
		assert.Equal(t, in.Name == "SMTP_PASSWORD", in.Required, "required flag lost for %s", in.Name)
	}

	// Values at stack and org scope both satisfy; per-env values satisfy too.
	now := time.Now().UTC()
	for _, v := range []repo.Variable{
		{OwnerKind: repo.OwnerStack, OwnerID: seed.Stack.ID, Name: "STRIPE_KEY", Value: "sk", Secret: true},
		{OwnerKind: repo.OwnerOrg, OwnerID: seed.Org.ID, Name: "SMTP_PASSWORD", Value: "h2", Secret: true},
		{OwnerKind: repo.OwnerStack, OwnerID: seed.Stack.ID, Name: "PINNED", Value: "p", Secret: true},
		{OwnerKind: repo.OwnerEnv, OwnerID: seed.Env.ID, Name: "SESSION_SECRET", Value: "s3", Secret: true},
	} {
		v.CreatedAt, v.UpdatedAt = now, now
		require.NoError(t, store.UpsertVariable(ctx, &v), "var %s", v.Name)
	}
	errs, warns, gen = pl.SecretIssues(ctx, seed.Stack, r, "", nil)
	assert.Empty(t, pl.DeclaredInputs(ctx, seed.Stack, r, "", nil), "all satisfied, want no inputs")
	assert.Empty(t, errs, "all satisfied, got errs=%v warns=%v gen=%v", errs, warns, gen)
	assert.Empty(t, warns, "all satisfied, got errs=%v warns=%v gen=%v", errs, warns, gen)
	assert.Empty(t, gen, "all satisfied, got errs=%v warns=%v gen=%v", errs, warns, gen)

	// env_versions: false + an env override = a loud error, not a silent skip.
	v := repo.Variable{OwnerKind: repo.OwnerEnv, OwnerID: seed.Env.ID, Name: "PINNED",
		Value: "sneaky", Secret: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.UpsertVariable(ctx, &v))
	errs, _, _ = pl.SecretIssues(ctx, seed.Stack, r, "", nil)
	require.Len(t, errs, 1, "errs = %v, want the env-override refusal", errs)
	assert.Contains(t, errs[0], "PINNED", "errs = %v, want the env-override refusal", errs)
	assert.Contains(t, errs[0], "env_versions", "errs = %v, want the env-override refusal", errs)
}

// Generation mints per env, marks the row secret, and never rotates: a second
// pass leaves the value untouched even when the length changes.
func TestEnsureSecretsGeneratesOnceAndStays(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	load := func(length int) *Resolved {
		r, err := Load([]byte(fmt.Sprintf("version: 1\nstack: s\nsecrets:\n  SESSION_SECRET:\n    default: generated\n"+
			"    length: %d\nenvironments:\n  prod:\n    tiles:\n      w: {image: nginx}\n", length)), nil)
		require.NoError(t, err)
		return r
	}
	a := Applier{Planner: Planner{Store: store}}
	require.NoError(t, a.ensureSecrets(ctx, seed.Stack, load(14), "", nil))
	vars, err := store.ListVariables(ctx, repo.OwnerEnv, seed.Env.ID)
	require.NoError(t, err, "env vars = %v, want the minted secret", vars)
	require.Len(t, vars, 1, "env vars = %v, want the minted secret", vars)
	require.Equal(t, "SESSION_SECRET", vars[0].Name, "minted row = %+v", vars[0])
	require.Len(t, vars[0].Value, 14, "minted row = %+v", vars[0])
	require.True(t, vars[0].Secret, "minted row = %+v", vars[0])
	first := vars[0].Value

	// A changed length must never rotate an existing value.
	require.NoError(t, a.ensureSecrets(ctx, seed.Stack, load(28), "", nil))
	vars, _ = store.ListVariables(ctx, repo.OwnerEnv, seed.Env.ID)
	require.Len(t, vars, 1, "generation was not stable: %v", vars)
	assert.Equal(t, first, vars[0].Value, "generation was not stable: %v", vars)
}

// A secret waiting to be generated is work, so the plan is not empty and not
// stored "clean". It used to be: minting only happens inside an apply, and the
// webhook path (maybeAutoApply) skips any plan that is not "pending", so a
// commit that added nothing but a generated secret left it unminted until some
// unrelated tile change dragged an apply along.
func TestGeneratedSecretMakesThePlanNonEmpty(t *testing.T) {
	p := &Plan{GenSecrets: []string{"SESSION_SECRET"}}
	assert.False(t, p.Empty(), "a pending mint read as an empty plan; the webhook path will skip it")
	assert.False(t, p.Destructive(), "generating a secret destroys nothing")
	got := p.Summary()
	assert.Contains(t, got, "1 secret to generate", "summary = %q, want the pending mint named", got)
	// And once it holds a value there is nothing left to do.
	assert.True(t, (&Plan{}).Empty(), "a plan with nothing in it is not empty")
}

// A store failure must not read as "everything is configured".
func TestMissingSecretsFailsLoud(t *testing.T) {
	p := &Plan{Warnings: []string{"could not check declared secrets: boom"}}
	assert.False(t, p.Destructive(), "a warning must not make a plan destructive")
	assert.True(t, p.Empty(), "warnings alone must not make a plan non-empty; they do not block an apply")
}

// An unset value's row names what will not deploy: the tiles that read it and
// everything behind those. That list is the point of the row; an unset secret
// is not one stalled tile, it is a chain.
func TestDeclaredInputsBlocked(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, true)

	r, err := Load([]byte(`version: 1
stack: s
secrets:
  SMTP_PASSWORD:
environments:
  production:
    tiles:
      web: {image: nginx, env: {P: "${{ stack.secrets.SMTP_PASSWORD }}"}}
      worker: {image: nginx, depends_on: [web:healthy]}
      db: {image: postgres}
`), nil)
	require.NoError(t, err)

	inputs := Planner{Store: store}.DeclaredInputs(ctx, seed.Stack, r, "", nil)
	require.Len(t, inputs, 1)
	assert.Equal(t, []string{"production/web", "production/worker"}, inputs[0].Blocked,
		"want the reader and its dependent, not db")
}
