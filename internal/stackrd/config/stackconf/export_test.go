package stackconf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const exportSource = `version: 1
stack: shop
ui_edits: stage
vars:
  LOG_LEVEL: info
secrets:
  SESSION_SECRET:
    default: generated
    length: 40
  STRIPE_KEY:
defaults:
  mem_limit_mb: 512
domains:
  - host: shop.example.com
    include_env_on_default: true
shared:
  sharedpg:
    type: managed
    engine: postgres
environments:
  production:
    tiles:
      web:
        image: nginx
        port: 8080
        env:
          PORT: "8080"
          PEM: "line1\nline2"
      data:
        type: volume
        attach: web
        path: /data
  staging:
    color: teal
    apply_policy: manual
    vars:
      LOG_LEVEL: debug
    defaults:
      mem_limit_mb: 256
    tiles:
      web:
        image: nginx
        port: 8080
`

// The export is the read side of the parser: a file exported from a loaded
// file has to load back to the same thing. Anything else means a panel-first
// user who exports gets a file that plans a diff against the stack it came
// from.
func TestExportRoundTrips(t *testing.T) {
	first, err := Load([]byte(exportSource), nil)
	require.NoError(t, err)

	out, err := ExportYAML(first)
	require.NoError(t, err, "export")
	second, err := Load(out, nil)
	require.NoError(t, err, "the exported file does not parse:\n%s", out)

	assert.Equal(t, first.Stack, second.Stack)
	assert.Equal(t, first.UIEdits, second.UIEdits)
	assert.Equal(t, first.EnvOrder, second.EnvOrder, "the ladder order is the file's, never sorted")
	assert.Equal(t, first.Vars, second.Vars)
	assert.Equal(t, first.Defaults, second.Defaults)
	assert.Equal(t, first.Domains, second.Domains)
	for _, env := range first.EnvOrder {
		a, b := first.Envs[env], second.Envs[env]
		assert.Equal(t, a.Color, b.Color, "env %s colour", env)
		assert.Equal(t, a.ApplyPolicy, b.ApplyPolicy, "env %s apply policy", env)
		assert.Equal(t, a.Vars, b.Vars, "env %s vars", env)
		assert.Equal(t, a.Defaults, b.Defaults, "env %s defaults", env)
		assert.Equal(t, a.Tiles, b.Tiles, "env %s tiles", env)
		assert.Equal(t, a.Secrets, b.Secrets, "env %s secrets", env)
	}
	// Shared instances live in the home environment, not on a rung.
	assert.Equal(t, first.Envs["stack"].Tiles, second.Envs["stack"].Tiles)
}

// A multi-line value (a PEM key, a JSON blob) used to make the canonical env
// form indistinguishable from two variables, so a tile carrying one never
// round-tripped to an empty diff.
func TestCanonEnvSurvivesMultilineValues(t *testing.T) {
	one := canonEnv("PEM=line1\nline2\nPORT=8080", nil)
	two := canonEnv("PORT=8080\nPEM=line1\nline2", nil)
	assert.Equal(t, one, two, "ordering leaked into the canonical form")

	// The two readings are different environments and must not canonicalize
	// the same way.
	assert.NotEqual(t,
		canonEnv("A=x\nB=y", nil),
		canonEnv("A=x\ny\nB=z", nil),
		"a newline inside a value reads as a second variable")
}
