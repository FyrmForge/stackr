package registry_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of the token server: what the client asks for is not what it
// gets. A scope outside the org's namespace is dropped, so the registry answers
// its own 401 rather than serving another tenant's image.
func TestGrantForStaysInsideTheNamespace(t *testing.T) {
	own := "repository:acme_shop_api:pull,push"
	other := "repository:rival_shop_api:pull"

	got := registry.GrantFor("acme", []string{own, other})
	require.Len(t, got, 1, "got %+v, want only the org's own repository", got)
	assert.Equal(t, "acme_shop_api", got[0].Name)
	assert.Equal(t, []string{"pull", "push"}, got[0].Actions)

	// A prefix that merely starts with the slug is a different org. Slugs are
	// [a-z0-9-] only, so the underscore is the boundary.
	assert.Empty(t, registry.GrantFor("acme", []string{"repository:acme-evil_x:pull"}),
		"acme-evil is not acme")
	// The slug itself is not a repository, and neither is the agent image.
	assert.Empty(t, registry.GrantFor("acme", []string{"repository:acme:pull"}))
	assert.Empty(t, registry.GrantFor("acme", []string{"repository:stkr-agent:pull"}))
	// Malformed scopes are dropped, not guessed at.
	assert.Empty(t, registry.GrantFor("acme", []string{"", "repository:x", "registry:catalog:*"}))

	// Several scopes arrive space-separated in one parameter.
	both := registry.GrantFor("acme", []string{own + " " + other})
	require.Len(t, both, 1, "got %+v", both)
	assert.Equal(t, "acme_shop_api", both[0].Name)
}

// The admin root credential reaches everything: the agent image and the
// garbage collector live outside every org's namespace.
func TestGrantAllKeepsEverything(t *testing.T) {
	got := registry.GrantAll([]string{"repository:stkr-agent:pull,push", "repository:acme_x:pull"})
	require.Len(t, got, 2, "got %+v", got)
	assert.Equal(t, "stkr-agent", got[0].Name)
}

// The agent's identity travels in a global service spec, so it is on every
// node for as long as the install lives. It reaches its own image, read-only,
// and nothing else.
func TestGrantAgentPullIsPullOnlyOnItsOwnImage(t *testing.T) {
	got := registry.GrantAgentPull([]string{"repository:stkr-agent:pull,push"})
	require.Len(t, got, 1, "got %+v", got)
	assert.Equal(t, "stkr-agent", got[0].Name)
	assert.Equal(t, []string{"pull"}, got[0].Actions, "the agent may not push its own image")

	assert.Empty(t, registry.GrantAgentPull([]string{"repository:acme_shop_api:pull"}),
		"the agent identity reached an org's image")
	assert.Empty(t, registry.GrantAgentPull([]string{"repository:stkr-agent:push"}),
		"push alone grants nothing")
	assert.Empty(t, registry.GrantAgentPull([]string{"registry:catalog:*"}))
	assert.Empty(t, registry.GrantAgentPull(nil))
}

// Derived, not stored: the registry password is the only input, so rotating it
// rotates the agent's secret with no row to migrate.
func TestAgentSecretFollowsTheRegistryPassword(t *testing.T) {
	a := registry.AgentSecret("pw-one")
	assert.Equal(t, a, registry.AgentSecret("pw-one"), "not deterministic")
	assert.NotEqual(t, a, registry.AgentSecret("pw-two"), "the password does not rotate it")
	assert.NotEmpty(t, a)
}

// A token has to verify against the certificate the registry mounts, so both
// halves come out of one directory and survive a restart.
func TestSignerPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := registry.LoadSigner(dir)
	require.NoError(t, err)
	tok, exp, err := first.Sign("acme/stackr", registry.GrantFor("acme", []string{"repository:acme_x:pull"}))
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.True(t, exp.After(exp.Add(-registry.TokenTTL)), "the token expires")

	second, err := registry.LoadSigner(dir)
	require.NoError(t, err)
	again, _, err := second.Sign("acme/stackr", nil)
	require.NoError(t, err)
	// Same key, so the header (which carries the certificate) is identical; a
	// regenerated keypair would leave every already-issued token unverifiable.
	assert.Equal(t, header(tok), header(again), "the signing keypair was not reused")
}

func header(token string) string {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			return token[:i]
		}
	}
	return token
}
