package stackconf

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const routingFile = `
version: 1
stack: owncloud
proxy:
  middlewares:
    oidc-discovery:
      replacePath:
        path: /index.php/apps/openidconnect/config
environments:
  production:
    tiles:
      owncloud:
        type: service
        image: owncloud/server:10
        port: 8080
        domains:
          - host: oc.example.com
          - host: oc.example.com
            rule: Host(` + "`oc.example.com`" + `) && Path(` + "`/.well-known/openid-configuration`" + `)
            priority: 100
            port: 9090
            middlewares: [oidc-discovery, auth/authelia]
`

func TestLoadRoutingKeys(t *testing.T) {
	r, err := Load([]byte(routingFile), nil)
	require.NoError(t, err)
	ds := r.Envs["production"].Tiles["owncloud"].Domains
	require.Len(t, ds, 2)
	assert.Equal(t, 100, ds[1].Priority)
	assert.Equal(t, 9090, ds[1].Port)
	assert.Equal(t, []string{"oidc-discovery", "auth/authelia"}, ds[1].Middlewares)
	assert.Contains(t, r.Middlewares, "oidc-discovery")

	for name, bad := range map[string]string{
		"rule without host": strings.Replace(routingFile, "          - host: oc.example.com\n            rule:", "          - auto: true\n            rule:", 1),
		"bad middleware":    strings.Replace(routingFile, "oidc-discovery:\n      replacePath", "bad/name:\n      replacePath", 1),
	} {
		_, err := Load([]byte(bad), nil)
		assert.Error(t, err, name)
	}
}

// Two entries on one host that differ only by rule are two routes, not a
// claim on the same one.
func TestDiffRuleEntriesShareAHost(t *testing.T) {
	r, err := Load([]byte(routingFile), nil)
	require.NoError(t, err)
	state := State{StackSlug: "owncloud", Envs: map[string]EnvState{},
		OrgMiddlewares: map[string]map[string]bool{"auth": {"authelia": true}}}
	p := Diff(r, state, DiffOpts{})
	assert.Empty(t, p.Errors)

	state.OrgMiddlewares = nil
	p = Diff(r, state, DiffOpts{})
	assert.Equal(t, []string{"env production: tile owncloud: no middleware auth/authelia"}, p.Errors)
}

func TestDiffMiddlewareDropInUse(t *testing.T) {
	r := &Resolved{Envs: map[string]ResolvedEnv{}}
	state := State{StackSlug: "auth",
		Middlewares:     RenderMiddlewares(map[string]RawMap{"authelia": {"forwardAuth": map[string]any{"address": "http://authelia:9091"}}, "old": {"headers": map[string]any{}}}),
		MiddlewareUsers: map[string][]string{"authelia": {"media/sonarr"}},
	}
	p := &Plan{}
	p.diffMiddlewares(r, state)
	require.Len(t, p.Errors, 1)
	assert.Contains(t, p.Errors[0], "media/sonarr")
	require.Len(t, p.Changes, 1)
	assert.Equal(t, Change{Kind: "delete", Env: "stack", Field: "middleware", Old: "old"}, p.Changes[0])
}

func TestDomainKeysRoundTrip(t *testing.T) {
	d := repo.Domain{Host: "a.example.com", ContainerPort: 9090, HTTPS: true, ForceHTTPS: true, Middlewares: "x\nauth/y", Priority: 5, Rule: "Host(`a.example.com`)"}
	got := domainsToConf([]repo.Domain{d}, nil, 8080)
	require.Len(t, got, 1)
	assert.Equal(t, domainLabel(got[0], d.Host), domainLabelDB(d))
	assert.Equal(t, 9090, got[0].Port)
}

// A rule entry on another tile's host is a claim on it: its priority would
// take that tile's traffic.
func TestDiffRuleCannotTakeAnotherTilesHost(t *testing.T) {
	r, err := Load([]byte(routingFile), nil)
	require.NoError(t, err)
	state := State{StackSlug: "owncloud", Envs: map[string]EnvState{},
		OrgMiddlewares: map[string]map[string]bool{"auth": {"authelia": true}},
		AllDomains:     []repo.Domain{{ID: "x", TileID: "other-tile", Host: "oc.example.com", Path: "/"}}}
	p := Diff(r, state, DiffOpts{})
	require.NotEmpty(t, p.Errors)
	assert.Contains(t, p.Errors[0], "already routes to another service")
}
