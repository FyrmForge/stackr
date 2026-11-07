package domain_test

import (
	"context"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
)

// TestMain gives the suite a DNS-free forward-auth check: a name counts
// as public, an IP literal as s3 judges it, except loopback (the Caddy tests
// run a provider on it). The guard tests below put the real check back.
func TestMain(m *testing.M) {
	domain.SetPublicAddr(func(raw string) (netip.Addr, bool) {
		u, _ := url.Parse(raw)
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
			if ip.IsLoopback() {
				return ip, true
			}
			return s3.PublicAddr(raw)
		}
		return netip.MustParseAddr("93.184.216.34"), true
	})
	os.Exit(m.Run())
}

func TestForwardAuthTargets(t *testing.T) {
	defer domain.SetPublicAddr(s3.PublicAddr)()
	l, _, web, _ := setup(t)
	l.WithOrgNames(func(context.Context, string) ([]string, error) { return []string{"stackr-auth-1"}, nil })
	for _, c := range []struct {
		name, url string
		ok        bool
	}{
		{"public ip", "https://93.184.216.34/verify", true},
		{"public ipv6", "https://[2606:2800:220:1::1]/verify", true},
		{"same-org tile", "http://stackr-auth-1:9091/api/verify", true},
		{"same-org tile, any case", "http://Stackr-Auth-1:9091/", true},
		{"other org's tile", "http://stackr-other-1:8080/", false},
		{"panel by name", "http://stackr:8080/", false},
		{"loopback", "http://127.0.0.1:2019/", false},
		{"loopback v6", "http://[::1]:2019/", false},
		{"v4-mapped loopback", "http://[::ffff:127.0.0.1]/", false},
		{"metadata", "http://169.254.169.254/latest/meta-data/", false},
		{"metadata v6", "http://[fd00:ec2::254]/", false},
		{"private LAN", "http://192.168.1.1/", false},
		{"docker bridge", "http://172.17.0.1:8080/", false},
		{"mesh", "http://10.208.0.1:8080/", false},
		{"shared space", "http://100.64.0.1/", false},
		{"unresolvable name", "http://nonexistent.invalid/", false},
	} {
		fa := &domain.ForwardAuth{URL: c.url}
		d, err := l.Attach(ctx, web, domain.Spec{Host: "g.example.com", Port: 80, Extras: domain.Extras{ForwardAuth: fa}}, false, nil)
		inv, isInv := errs.IsInvalid(err)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case !c.ok && (!isInv || inv.Field != "proxy.forward_auth.url"):
			t.Errorf("%s: %v, want a forward_auth.url refusal", c.name, err)
		}
		if err == nil {
			must(t, l.Detach(ctx, d)) // free the host for the next case
		}
	}
}

// The build path re-checks: a forward-auth row that got into the store past
// Attach leaves its tile out of the config, it never serves open.
func TestForwardAuthBuildRefusesPrivate(t *testing.T) {
	defer domain.SetPublicAddr(s3.PublicAddr)()
	off := install
	off.TLSOff = true
	for _, c := range []struct {
		url   string
		names []string
		ok    bool
	}{
		{"https://93.184.216.34/v", nil, true},
		{"http://stackr-auth-1:9091/", []string{"stackr-auth-1"}, true},
		{"http://stackr-other-1:9091/", []string{"stackr-auth-1"}, false},
		{"http://192.168.1.1/", nil, false},
		{"http://169.254.169.254/", nil, false},
	} {
		tr := one(row("app.example.com", domain.Extras{ForwardAuth: &domain.ForwardAuth{URL: c.url}}))
		tr[0].OrgNames = c.names
		raw, err := domain.Build(off, tr, nil, expand)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.url, err)
		}
		if !c.ok && (err == nil || strings.Contains(string(raw), "app.example.com")) {
			t.Errorf("%s: err=%v, route still in the config", c.url, err)
		}
	}
}
