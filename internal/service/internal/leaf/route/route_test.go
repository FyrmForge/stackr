package route_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func TestCreateRules(t *testing.T) {
	l := route.New(servicetest.Store(t).Routes)
	for _, c := range []struct {
		name         string
		s            route.Spec
		tlsOn, dns01 bool
		field        string // "" = accepted
		target       string
	}{
		{"passthrough default port", route.Spec{Host: "pve.io", Mode: "passthrough", Target: "10.0.0.5"}, true, false, "", "10.0.0.5:443"},
		{"http default port", route.Spec{Host: "hole.io", Mode: "http", Target: "10.0.0.6"}, true, false, "", "10.0.0.6:80"},
		{"https insecure", route.Spec{Host: "nas.io", Mode: "https", Target: "nas:8443", Insecure: true}, true, false, "", "nas:8443"},
		{"wildcard passthrough needs no dns", route.Spec{Host: "*.fyrm.io", Mode: "passthrough", Target: "10.0.0.9"}, true, false, "", "10.0.0.9:443"},
		{"wildcard http needs dns", route.Spec{Host: "*.web.io", Mode: "http", Target: "10.0.0.9"}, true, false, "host", ""},
		{"wildcard http with dns", route.Spec{Host: "*.web.io", Mode: "http", Target: "10.0.0.9"}, true, true, "", "10.0.0.9:80"},
		{"passthrough needs tls", route.Spec{Host: "off.io", Mode: "passthrough", Target: "10.0.0.5"}, false, false, "mode", ""},
		{"insecure only on https", route.Spec{Host: "ins.io", Mode: "http", Target: "x", Insecure: true}, true, false, "insecure", ""},
		{"bad mode", route.Spec{Host: "m.io", Mode: "tcp", Target: "x"}, true, false, "mode", ""},
		{"scheme in host", route.Spec{Host: "https://h.io", Mode: "http", Target: "x"}, true, false, "host", ""},
		{"address host", route.Spec{Host: "10.0.0.1", Mode: "http", Target: "x"}, true, false, "host", ""},
		{"bad port", route.Spec{Host: "p.io", Mode: "http", Target: "x:99999"}, true, false, "target", ""},
		{"placeholder in target", route.Spec{Host: "b.io", Mode: "http", Target: "{env.DNS_API_TOKEN}:80"}, true, false, "target", ""},
		{"closing brace in target", route.Spec{Host: "c.io", Mode: "http", Target: "x}"}, true, false, "target", ""},
		{"url target", route.Spec{Host: "u.io", Mode: "http", Target: "http://x"}, true, false, "target", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := l.Create(ctx, c.s, nil, c.tlsOn, c.dns01)
			if c.field == "" {
				if err != nil || r.Target != c.target {
					t.Fatalf("= %+v, %v; want target %s", r, err, c.target)
				}
				return
			}
			inv, ok := errs.IsInvalid(err)
			if !ok || inv.Field != c.field {
				t.Fatalf("err = %v, want invalid %s", err, c.field)
			}
		})
	}
}

// One host, one owner: the other tables' hosts come in as arguments, the
// leaf's own table and wildcards are checked here.
func TestSquat(t *testing.T) {
	l := route.New(servicetest.Store(t).Routes)
	pass := route.Spec{Host: "*.fyrm.io", Mode: "passthrough", Target: "10.0.0.9"}
	if _, err := l.Create(ctx, pass, []string{"app.example.com"}, true, false); err != nil {
		t.Fatal(err)
	}
	for host, taken := range map[string][]string{
		"app.example.com": {"APP.example.com"}, // a tile domain or resource, any case
		"fyrm.io":         nil,                 // not covered by *.fyrm.io: fine
		"a.fyrm.io":       nil,                 // covered by our wildcard
		"*.fyrm.io":       nil,                 // twin
		"*.a.fyrm.io":     nil,                 // one label, not two: *.fyrm.io leaves it alone
	} {
		_, err := l.Create(ctx, route.Spec{Host: host, Mode: "http", Target: "x"}, taken, true, true)
		_, conflict := errs.IsConflict(err)
		if want := host != "fyrm.io" && host != "*.a.fyrm.io"; conflict != want {
			t.Errorf("%s: err = %v, conflict %v, want %v", host, err, conflict, want)
		}
	}
	// A wildcard route over a taken host is refused too.
	_, err := l.Create(ctx, route.Spec{Host: "*.example.com", Mode: "passthrough", Target: "x"}, []string{"app.example.com"}, true, false)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "example.com") {
		t.Errorf("wildcard over a tile domain = %v, want conflict", err)
	}
	// Covers is the reverse check for a tile domain (fyrm.io was added above).
	for host, want := range map[string]bool{"a.fyrm.io": true, "x.y.fyrm.io": false, "fyrm.io": true, "other.io": false} {
		got, err := l.Covers(ctx, host)
		if err != nil || got != want {
			t.Errorf("Covers(%s) = %v, %v, want %v", host, got, err, want)
		}
	}
}

// A wildcard route sits one label above a domain resource and would take
// every auto host under it: the resource host and *.<resource> refuse each
// other, in both orders.
func TestWildcardOverResource(t *testing.T) {
	l := route.New(servicetest.Store(t).Routes)
	wild := route.Spec{Host: "*.res.io", Mode: "passthrough", Target: "10.0.0.9"}
	_, err := l.Create(ctx, wild, []string{"res.io"}, true, false)
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("wildcard over a resource host = %v, want conflict", err)
	}
	if _, err = l.Create(ctx, wild, nil, true, false); err != nil {
		t.Fatal(err)
	}
	held, err := l.Covers(ctx, "res.io")
	if err != nil || !held {
		t.Errorf("Covers(res.io) = %v, %v, want true", held, err)
	}
}
