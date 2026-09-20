package stackconf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Two rules the panel and the API have always enforced and config-as-code did
// not, so a stack file could write a route that cannot work and the plan went
// green: a wildcard host with no DNS provider (the certificate never issues)
// and a service tile with no port anywhere (the route renders http://alias:0).
//
// Both now come from service.CheckWildcardHTTPS / service.DomainPort, called
// from DomainService.plan and from the planner, so there is one copy.

const wildcardFile = `
version: 1
stack: shop
environments:
  production:
    tiles:
      web:
        type: service
        image: nginx:1
        port: 8080
        domains:
          - host: "*.shop.example.com"
`

const noPortFile = `
version: 1
stack: shop
environments:
  production:
    tiles:
      web:
        type: service
        image: nginx:1
        domains:
          - host: shop.example.com
`

const redirectNoPortFile = `
version: 1
stack: shop
environments:
  production:
    tiles:
      web:
        type: service
        image: nginx:1
        domains:
          - host: old.example.com
            redirect_to: https://shop.example.com
`

func planErrors(t *testing.T, file string, opts DiffOpts) []string {
	t.Helper()
	r, err := Load([]byte(file), nil)
	require.NoError(t, err)
	return Diff(r, State{}, opts).Errors
}

func TestWildcardDomainNeedsADNSProvider(t *testing.T) {
	errs := planErrors(t, wildcardFile, DiffOpts{})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "wildcard HTTPS needs a DNS provider")

	// Configured: the same file plans clean.
	assert.Empty(t, planErrors(t, wildcardFile, DiffOpts{DNSProvider: "cloudflare"}))

	// https: false never asks for a certificate, so the rule does not apply.
	assert.NoError(t, service.CheckWildcardHTTPS("*.shop.example.com", false, false))
}

func TestServiceDomainNeedsAPort(t *testing.T) {
	errs := planErrors(t, noPortFile, DiffOpts{})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "container port required")

	// A redirect never proxies, so it is allowed without one — and lands on
	// 80 rather than 0, which is what http://alias:0 used to come from.
	assert.Empty(t, planErrors(t, redirectNoPortFile, DiffOpts{}))
	port, err := service.DomainPort(0, 0, "https://shop.example.com")
	require.NoError(t, err)
	assert.Equal(t, 80, port)
}

func TestDomainPortPrefersTheDomainsOwn(t *testing.T) {
	port, err := service.DomainPort(9090, 8080, "")
	require.NoError(t, err)
	assert.Equal(t, 9090, port)

	port, err = service.DomainPort(0, 8080, "")
	require.NoError(t, err)
	assert.Equal(t, 8080, port)
}

// The rules run on both walks. A tile the plan creates goes through
// claimHosts; one that already exists goes through diffDomains and never
// reaches claimHosts, so a check wired into only one of them covers exactly
// the half nobody's existing stack is in.
func TestTheRulesRunOnTheUpdateWalkToo(t *testing.T) {
	r, err := Load([]byte(wildcardFile), nil)
	require.NoError(t, err)
	state := State{Envs: map[string]EnvState{"production": {Tiles: map[string]TileState{
		"web": {
			Tile:    repo.Tile{ID: "web-id", Kind: "service", TileConfig: repo.TileConfig{ImageRef: "nginx:1", ContainerPort: 8080}},
			Domains: []repo.Domain{{TileID: "web-id", Host: "*.shop.example.com", ContainerPort: 8080}},
		},
	}}}}

	errs := Diff(r, state, DiffOpts{}).Errors
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "wildcard HTTPS needs a DNS provider")

	assert.Empty(t, Diff(r, state, DiffOpts{DNSProvider: "cloudflare"}).Errors)
}
