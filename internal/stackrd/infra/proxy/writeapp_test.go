package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func writeAndRead(t *testing.T, app *repo.Tile, domains []repo.Domain) string {
	t.Helper()
	p := &Proxy{dir: t.TempDir(), acmeEmail: "ops@example.com"}
	require.NoError(t, os.MkdirAll(filepath.Join(p.dir, "dynamic"), 0o755))
	require.NoError(t, p.WriteApp(app, domains))
	b, err := os.ReadFile(filepath.Join(p.dir, "dynamic", "app-"+app.ID+".yml"))
	require.NoError(t, err)
	return string(b)
}

func TestWriteAppExtras(t *testing.T) {
	app := &repo.Tile{
		ID:            "abcdefgh-1234",
		BasicAuthUser: "admin",
		BasicAuthHash: "$2a$10$hash",
		SecHeaders:    true,
	}
	got := writeAndRead(t, app, []repo.Domain{
		{ID: "d1", Host: "app.example.com", ContainerPort: 8080, HTTPS: true},
		{ID: "d2", Host: "www.example.com", ContainerPort: 8080, HTTPS: true, RedirectTo: "app.example.com"},
		{ID: "d3", Host: "*.example.com", ContainerPort: 8080, HTTPS: true},
	})

	for _, want := range []string{
		"basicAuth",
		`"admin:$2a$10$hash"`,
		"stsSeconds: 31536000",
		"middlewares: [app-abcdefgh-auth, app-abcdefgh-hdr]",
		"redirectRegex",
		`regex: "^https?://www\\.example\\.com/(.*)"`,
		`replacement: "https://app.example.com/${1}"`,
		"middlewares: [app-abcdefgh-1-rd]",
		"HostRegexp",
		`sans: ["*.example.com"]`,
		"certResolver: ledns",
	} {
		assert.Contains(t, got, want)
	}
	// The redirect domain must not inherit the auth/header middlewares.
	assert.NotContains(t, got, "middlewares: [app-abcdefgh-auth, app-abcdefgh-hdr, app-abcdefgh-1-rd]",
		"redirect router got auth middlewares")
}

func TestWriteAppCustomCert(t *testing.T) {
	app := &repo.Tile{ID: "abcdefgh-1234"}
	got := writeAndRead(t, app, []repo.Domain{
		{ID: "d1", Host: "corp.example.com", ContainerPort: 80, HTTPS: true, CertPEM: "CERT", KeyPEM: "KEY"},
	})
	assert.Contains(t, got, "tls: {}", "custom cert config missing")
	assert.Contains(t, got, "certFile: /etc/traefik/certs/d1.crt", "custom cert config missing")
	assert.NotContains(t, got, "certResolver", "custom-cert domain must not use ACME")
}

func TestWriteAppOverride(t *testing.T) {
	app := &repo.Tile{ID: "abcdefgh-1234", TraefikOverride: "http:\n  routers: {}\n"}
	got := writeAndRead(t, app, nil)
	assert.Equal(t, app.TraefikOverride, got, "override not written verbatim")
}

func TestPruneOrphanAppsKeepsEverythingElse(t *testing.T) {
	p := &Proxy{dir: t.TempDir()}
	dyn := filepath.Join(p.dir, "dynamic")
	require.NoError(t, os.MkdirAll(dyn, 0o755))
	files := []string{
		"app-live-tile.yml", // tile exists → keep
		"app-dead-tile.yml", // tile gone → remove
		"_middlewares.yml",  // hand-managed → never touched
		"registry.yml",      // ditto
		"app-notes.txt",     // not a route file
	}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dyn, f), []byte("x"), 0o644))
	}

	require.NoError(t, p.pruneOrphanApps([]repo.Tile{{ID: "live-tile"}}), "prune")

	for _, f := range files {
		_, err := os.Stat(filepath.Join(dyn, f))
		gone := os.IsNotExist(err)
		if f == "app-dead-tile.yml" {
			assert.True(t, gone, "%s should have been removed", f)
		} else {
			assert.False(t, gone, "%s should have been kept", f)
		}
	}
}

func TestWriteAppTLSOff(t *testing.T) {
	p := &Proxy{dir: t.TempDir(), noTLS: true} // STACKR_TLS=off
	require.NoError(t, os.MkdirAll(filepath.Join(p.dir, "dynamic"), 0o755))
	app := &repo.Tile{ID: "abcdefgh-1234"}
	domains := []repo.Domain{
		{ID: "d0", Host: "app.example.com", ContainerPort: 8080, HTTPS: true},
		{ID: "d1", Host: "corp.example.com", ContainerPort: 80, HTTPS: true, CertPEM: "CERT", KeyPEM: "KEY"},
	}
	require.NoError(t, p.WriteApp(app, domains))
	b, err := os.ReadFile(filepath.Join(p.dir, "dynamic", "app-"+app.ID+".yml"))
	require.NoError(t, err)
	out := string(b)
	assert.NotContains(t, out, "websecure")
	assert.NotContains(t, out, "certResolver")
	assert.NotContains(t, out, "https-redirect")
	assert.NotContains(t, out, "tls:")
	assert.Contains(t, out, "entryPoints: [web]")
}

// The panel routes itself: a wipe must not leave the operator with a panel
// they can only reach by hostname if they can first reach the panel.
func TestWritePanel(t *testing.T) {
	newP := func(host string) *Proxy {
		p := &Proxy{dir: t.TempDir(), panelHost: host, panelURL: "http://stackr-panel:8080"}
		require.NoError(t, os.MkdirAll(filepath.Join(p.dir, "dynamic"), 0o755))
		return p
	}
	read := func(p *Proxy) string {
		b, err := os.ReadFile(filepath.Join(p.dir, "dynamic", "panel.yml"))
		if os.IsNotExist(err) {
			return ""
		}
		require.NoError(t, err)
		return string(b)
	}

	p := newP("panel.example.com")
	require.NoError(t, p.writePanel())
	out := read(p)
	assert.Contains(t, out, "Host(`panel.example.com`)")
	assert.Contains(t, out, "http://stackr-panel:8080")
	assert.Contains(t, out, "certResolver: le")
	assert.Contains(t, out, "middlewares: [https-redirect]")

	// A LAN box has no name to route and no certificate to get. Same for an
	// unset BASE_URL, and a file an earlier boot wrote has to go.
	for _, host := range []string{"", "localhost", "192.168.1.20"} {
		p := newP(host)
		require.NoError(t, os.WriteFile(filepath.Join(p.dir, "dynamic", "panel.yml"), []byte("stale"), 0o644))
		require.NoError(t, p.writePanel())
		assert.Empty(t, read(p), "host %q must not get a route", host)
	}
}

// Serving TLS and bouncing plain HTTP onto it are two questions. A host that
// must still answer on http (a legacy client, a check that cannot follow a
// redirect) can have a certificate without being redirected.
func TestForceHTTPSIsSeparateFromServingTLS(t *testing.T) {
	both := writeAndRead(t, &repo.Tile{ID: "aaaabbbbccccdddd", ContainerPort: 80},
		[]repo.Domain{{ID: "d1", Host: "a.example.com", ContainerPort: 80, HTTPS: true, ForceHTTPS: true}})
	assert.Contains(t, both, "middlewares: [https-redirect]", "force_https on should bounce plain http")

	noRedirect := writeAndRead(t, &repo.Tile{ID: "aaaabbbbccccdddd", ContainerPort: 80},
		[]repo.Domain{{ID: "d1", Host: "a.example.com", ContainerPort: 80, HTTPS: true}})
	assert.NotContains(t, noRedirect, "https-redirect",
		"force_https off must leave the http router serving the tile")
	assert.Contains(t, noRedirect, "entryPoints: [websecure]", "TLS is still served")
	assert.Contains(t, noRedirect, "entryPoints: [web]", "and so is plain http")
}

func TestWriteAppRouteKeys(t *testing.T) {
	app := &repo.Tile{ID: "abcdefgh-1234", StackID: "stack123-0000"}
	got := writeAndRead(t, app, []repo.Domain{
		{ID: "d1", Host: "oc.example.com", ContainerPort: 8080, HTTPS: true},
		{ID: "d2", Host: "oc.example.com", ContainerPort: 9090, HTTPS: true, Priority: 100,
			Rule: "Host(`oc.example.com`) && Path(`/.well-known/openid-configuration`)", Middlewares: "oidc-discovery"},
	})
	assert.Contains(t, got, `rule: "Host(`+"`oc.example.com`"+`) && Path(`+"`/.well-known/openid-configuration`"+`)"`)
	assert.Contains(t, got, "priority: 100")
	assert.Contains(t, got, "middlewares: [stk-stack123-oidc-discovery@file]")
	assert.Contains(t, got, "certResolver: le", "the rule entry still gets its host's certificate")
	assert.Contains(t, got, "tile-abcdefgh:9090")
}

func TestWriteStackMiddlewares(t *testing.T) {
	p := &Proxy{dir: t.TempDir()}
	s := &repo.Stack{ID: "stack123-0000", Slug: "auth",
		ProxyMiddlewares: "authelia:\n  forwardAuth:\n    address: http://authelia:9091/api/authz/forward-auth\n"}
	require.NoError(t, p.WriteStackMiddlewares(s))
	path := filepath.Join(p.dir, "dynamic", "stack-"+s.ID+".yml")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), "stk-stack123-authelia:")
	assert.Contains(t, string(b), "address: http://authelia:9091/api/authz/forward-auth")

	s.ProxyMiddlewares = ""
	require.NoError(t, p.WriteStackMiddlewares(s))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}
