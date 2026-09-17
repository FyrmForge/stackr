package installer

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChecks(t *testing.T) {
	root := CheckRoot
	host := func(s string) (string, error) { return CheckHost(s, "fyrmforge.dev") }
	port := func(s string) (string, error) { return s, CheckPort(s, nil) }
	for _, c := range []struct {
		name  string
		check func(string) (string, error)
		in    string
		want  string // "" with ok false means the check must refuse
		ok    bool
	}{
		{"root", root, "fyrmforge.dev", "fyrmforge.dev", true},
		{"root", root, "https://FyrmForge.dev/x", "fyrmforge.dev", true},
		{"root", root, "localhost", "", false},
		{"root", root, "192.168.1.20", "", false},
		{"root", root, "1.2.3", "", false},
		{"root", root, "dev", "", false},
		{"root", root, "fyrm forge.dev", "", false},
		{"root", root, "fyrm_forge.dev", "", false},
		{"root", root, "-fyrm.dev", "", false},
		{"root", root, "fyrm-.dev", "", false},
		{"root", root, "a..dev", "", false},
		{"root", root, strings.Repeat("a", 64) + ".dev", "", false},
		{"root", root, "", "", false},

		{"host", host, "stkr.fyrmforge.dev", "stkr.fyrmforge.dev", true},
		{"host", host, "fyrmforge.dev", "fyrmforge.dev", true},
		{"host", host, "a.b.fyrmforge.dev", "a.b.fyrmforge.dev", true},
		{"host", host, "xfyrmforge.dev", "", false},
		{"host", host, "other.dev", "", false},
		{"host", host, "192.168.1.20", "", false},

		{"proxy", CheckProxies, "", "", true},
		{"proxy", CheckProxies, "192.168.1.100", "192.168.1.100/32", true},
		{"proxy", CheckProxies, "192.168.1.100, 10.0.0.0/8 192.168.1.100", "192.168.1.100/32,10.0.0.0/8", true},
		{"proxy", CheckProxies, "2001:db8::1", "2001:db8::1/128", true},
		{"proxy", CheckProxies, "0.0.0.0/0", "", false},
		{"proxy", CheckProxies, "::/0", "", false},
		{"proxy", CheckProxies, "10.0.0.0/33", "", false},
		{"proxy", CheckProxies, "10.0.0.0/", "", false},
		{"proxy", CheckProxies, "2001:db8:::1", "", false},
		{"proxy", CheckProxies, "nope", "", false},

		{"email", CheckEmail, "ops@example.com", "ops@example.com", true},
		{"email", CheckEmail, "ops@Example.COM", "ops@example.com", true},
		{"email", CheckEmail, "", "", false},
		{"email", CheckEmail, "o ps@example.com", "", false},
		{"email", CheckEmail, "ops@example", "", false},
		{"email", CheckEmail, "@example.com", "", false},
		{"email", CheckEmail, "ops@@example.com", "", false},
		{"email", CheckEmail, "ops@localhost", "", false},
		{"email", CheckEmail, "ops@1.2.3.4", "", false},

		{"data dir", CheckDataDir, "/var/lib/stackr/", "/var/lib/stackr", true},
		{"data dir", CheckDataDir, "var/lib", "", false},
		{"data dir", CheckDataDir, "/", "", false},
		{"data dir", CheckDataDir, "/var/lib/st ackr", "", false},
		{"data dir", CheckDataDir, "/var/lib/a,b", "", false},
		{"data dir", CheckDataDir, "/var/../etc", "", false},

		{"port", port, "443", "443", true},
		{"port", port, "0", "", false},
		{"port", port, "080", "", false},
		{"port", port, "-1", "", false},
		{"port", port, "65536", "", false},
		{"port", port, "8x", "", false},
	} {
		got, err := c.check(c.in)
		if c.ok {
			if assert.NoError(t, err, "%s %q", c.name, c.in) {
				assert.Equal(t, c.want, got, "%s %q", c.name, c.in)
			}
		} else {
			assert.Error(t, err, "%s %q", c.name, c.in)
		}
	}

	assert.Error(t, CheckHTTPSPort("80", "80", nil))
	assert.Error(t, CheckPort("80", func(string) bool { return true }))
}

func TestPickPool(t *testing.T) {
	got, err := pickPool(poolCandidates, []string{"default", "192.168.1.0/24", "10.250.3.0/24"})
	require.NoError(t, err)
	assert.Equal(t, "10.252.0.0/15", got, "a subnet inside the first candidate rules it out")

	_, err = pickPool(poolCandidates, []string{"10.0.0.0/8"})
	assert.Error(t, err, "a supernet overlaps every candidate")
}

func args(t *testing.T, flags ...string) *Answers {
	t.Helper()
	a, _, err := parseFlags(append([]string{"--version", "0.1.5", "--yes"}, flags...), io.Discard)
	require.NoError(t, err)
	require.NoError(t, a.Check(nil))
	return a
}

// env pulls one --env value out of a service create.
func env(argv []string, key string) (string, bool) {
	for i, a := range argv {
		if i > 0 && argv[i-1] == "--env" && strings.HasPrefix(a, key+"=") {
			return strings.TrimPrefix(a, key+"="), true
		}
	}
	return "", false
}

func TestServiceArgs(t *testing.T) {
	const img = "ghcr.io/fyrmforge/stackr:0.1.5"

	t.Run("domain with https", func(t *testing.T) {
		a := args(t, "--domain", "Example.com", "--proxy", "192.168.1.100", "--email", "ops@example.com")
		argv := serviceArgs(a, img)
		for k, want := range map[string]string{
			"BASE_URL":            "https://stkr.example.com",
			"STACKR_TLS":          "on",
			"ROOT_DOMAIN":         "example.com",
			"TRUSTED_PROXY_CIDRS": "192.168.1.100/32",
			"ACME_EMAIL":          "ops@example.com",
			"TRAEFIK_HTTPS_PORT":  "443",
		} {
			got, _ := env(argv, k)
			assert.Equal(t, want, got, k)
		}
		assert.NotContains(t, argv, "--publish", "traefik routes the panel")
		assert.Equal(t, img, argv[len(argv)-1])
	})

	t.Run("domain without https", func(t *testing.T) {
		a := args(t, "--domain", "example.com", "--panel-host", "panel.example.com", "--https=false", "--http-port", "8000")
		argv := serviceArgs(a, img)
		got, _ := env(argv, "BASE_URL")
		assert.Equal(t, "http://panel.example.com:8000", got)
		got, _ = env(argv, "STACKR_TLS")
		assert.Equal(t, "off", got)
		got, _ = env(argv, "ACME_EMAIL")
		assert.Empty(t, got, "no Let's Encrypt email without HTTPS")
		_, ok := env(argv, "TRAEFIK_HTTPS_PORT")
		assert.False(t, ok, "no HTTPS port with HTTPS off")
	})

	t.Run("the panel never publishes a port", func(t *testing.T) {
		argv := serviceArgs(args(t, "--domain", "example.com", "--email", "ops@example.com"), img)
		assert.NotContains(t, strings.Join(argv, " "), "--publish", "traefik is the only way in")
	})
}

func TestParseFlagsYes(t *testing.T) {
	for _, flags := range [][]string{
		{"--version", "0.1.5", "--yes"},
		{"--yes", "--domain", "example.com"}, // a dev build has no release to install
		{"--version", "0.1.5", "stray"},
		{"--version", "latest", "--yes", "--domain", "example.com"}, // a dev build has no release behind "latest"
		{"--version", "0.1.5 ; rm -rf /", "--yes", "--domain", "example.com"},
	} {
		_, _, err := parseFlags(flags, io.Discard)
		assert.Error(t, err, "%v", flags)
	}

	a, _, err := parseFlags([]string{"--version", "0.1.5", "--yes", "--domain", "example.com"}, io.Discard)
	require.NoError(t, err)
	assert.Error(t, a.Check(nil), "HTTPS on needs an email")
}

func TestDNSWarnings(t *testing.T) {
	a := args(t, "--domain", "example.com", "--email", "ops@example.com")
	resolves := func(names ...string) func(context.Context, string) ([]string, error) {
		return func(_ context.Context, name string) ([]string, error) {
			for _, n := range names {
				if name == n || strings.HasSuffix(name, ".example.com") && n == "*" {
					return []string{"1.2.3.4"}, nil
				}
			}
			return nil, errors.New("no such host")
		}
	}
	assert.Empty(t, dnsWarnings(context.Background(), a, resolves("*")))

	w := strings.Join(dnsWarnings(context.Background(), a, resolves("stkr.example.com")), "\n")
	assert.Contains(t, w, "*.example.com")
	assert.NotContains(t, w, "stkr.example.com does not resolve")

	a.HTTPS = false
	assert.Empty(t, dnsWarnings(context.Background(), a, resolves()), "no certificates, nothing to check")
}
