package domain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup seeds two tiles in one env.
func setup(t *testing.T) (*domain.Leaf, *dockerfake.Fake, string, string) {
	st := servicetest.Store(t)
	org, stack, env := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{
		ID:        org,
		Name:      org,
		Slug:      org[:8],
		EnvColors: "{}",
		Settings:  "{}",
		CreatedAt: now,
	}))
	must(t, st.Stacks.Create(ctx, store.Stack{
		ID:        stack,
		OrgID:     org,
		Name:      "s",
		Slug:      "s",
		Settings:  "{}",
		Domains:   "[]",
		CreatedAt: now,
	}))
	must(t, st.Environments.Create(ctx, store.Environment{
		ID:         env,
		StackID:    stack,
		Name:       "dev",
		Slug:       "dev",
		Type:       "static",
		Settings:   "{}",
		Network:    "n",
		FromKind:   "branch",
		FromBranch: "main",
		CreatedAt:  now,
	}))
	var ids []string
	for _, s := range []string{"web", "api"} {
		id := uuid.NewString()
		must(t, st.Tiles.Create(ctx, store.Tile{
			ID:            id,
			StackID:       stack,
			EnvironmentID: env,
			Name:          s,
			Slug:          s,
			Kind:          "image",
			UpdatePolicy:  "manual",
			Replicas:      1,
			CreatedAt:     now,
			UpdatedAt:     now,
		}))
		ids = append(ids, id)
	}
	fake := dockerfake.New()
	return domain.New(st.Domains, fake, "stackr-proxy"), fake, ids[0], ids[1]
}

func no() *bool {
	b := false
	return &b
}

// B31: nil means on, on both Attach and Update; an explicit false stays false.
func TestHTTPSNilIsOn(t *testing.T) {
	l, _, web, _ := setup(t)
	d, err := l.Attach(ctx, web, domain.Spec{Host: "App.Example.com", Port: 80}, false, nil)
	if err != nil || !d.HTTPS || !d.ForceHTTPS || d.Host != "app.example.com" {
		t.Fatalf("nil = %+v, %v", d, err)
	}
	d, err = l.Update(ctx, d, domain.Spec{
		Host:       d.Host,
		Port:       80,
		HTTPS:      no(),
		ForceHTTPS: no(),
	}, false)
	if err != nil || d.HTTPS || d.ForceHTTPS {
		t.Fatalf("false = %+v, %v", d, err)
	}
	if got, _ := l.Get(ctx, d.ID); got.HTTPS || got.ForceHTTPS {
		t.Errorf("stored %+v", got)
	}
	d, _ = l.Update(ctx, d, domain.Spec{Host: d.Host, Port: 80}, false)
	if got, _ := l.Get(ctx, d.ID); !got.HTTPS || !got.ForceHTTPS {
		t.Errorf("update with nil = %+v, want on", got)
	}
}

func TestAttachRules(t *testing.T) {
	l, _, web, api := setup(t)
	if _, err := l.Attach(ctx, web, domain.Spec{Host: "a.example.com", Path: "/", Port: 80}, false, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		tile  string
		s     domain.Spec
		dns01 bool
		field string // "" = conflict
	}{
		{"bad host", api, domain.Spec{Host: "a b", Port: 80}, false, "host"},
		{"wildcard without dns-01", api, domain.Spec{Host: "*.example.com", Port: 80}, false, "host"},
		{"no port", api, domain.Spec{Host: "b.example.com"}, false, "port"},
		{"bad path", api, domain.Spec{Host: "b.example.com", Path: "api", Port: 80}, false, "path"},
		{"bad method", api, domain.Spec{
			Host:   "b.example.com",
			Port:   80,
			Extras: domain.Extras{Methods: []string{"propfind"}},
		}, false, "proxy.methods"},
		{"auth without user", api, domain.Spec{
			Host:   "b.example.com",
			Port:   80,
			Extras: domain.Extras{BasicAuth: &domain.BasicAuth{}},
		}, false, "proxy.basic_auth.user"},
		{"raw not json", api, domain.Spec{Host: "b.example.com", Port: 80, RawCaddy: "{"}, false, "raw_caddy"},
		{"taken host", api, domain.Spec{Host: "A.example.com", Port: 80}, false, ""},
	} {
		_, err := l.Attach(ctx, c.tile, c.s, c.dns01, nil)
		inv, isInv := errs.IsInvalid(err)
		_, isConf := errs.IsConflict(err)
		if c.field == "" && !isConf || c.field != "" && (!isInv || inv.Field != c.field) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, s := range []domain.Spec{
		{Host: "*.example.com", Port: 80, HTTPS: no()},         // no certificate needed
		{Host: "a.example.com", Path: "/api", Port: 80},        // same host, other path
		{Host: "old.example.com", RedirectTo: "a.example.com"}, // a redirect needs no port
	} {
		if _, err := l.Attach(ctx, api, s, false, nil); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	if _, err := l.Attach(ctx, api, domain.Spec{Host: "*.x.io", Port: 80}, true, nil); err != nil {
		t.Errorf("wildcard with dns-01: %v", err)
	}
}

// The first domain opens the ingress network (proxy + running replicas),
// the last one disconnects every member and removes it.
func TestIngressLifecycle(t *testing.T) {
	l, fake, web, _ := setup(t)
	net := domain.Ingress(web)
	a, err := l.Attach(ctx, web, domain.Spec{Host: "a.io", Port: 80}, false, []string{"web-r1"})
	must(t, err)
	b, err := l.Attach(ctx, web, domain.Spec{Host: "b.io", Port: 80}, false, []string{"web-r1"})
	must(t, err)
	if b.Position != 1 {
		t.Errorf("position = %d", b.Position)
	}
	must(t, l.Detach(ctx, a))
	fake.Members = map[string][]string{net: {"stackr-proxy", "web-r1"}}
	must(t, l.Detach(ctx, b))

	var got []string
	for _, c := range fake.Calls() {
		got = append(got, c.String())
	}
	want := []string{
		"EnsureNetwork(" + net + ")",
		"Connect(" + net + ", stackr-proxy, )",
		"Connect(" + net + ", web-r1, )",
		"NetworkMembers(" + net + ")",
		"Disconnect(" + net + ", stackr-proxy)",
		"Disconnect(" + net + ", web-r1)",
		"RemoveNetwork(" + net + ")",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	fake.Err = map[string]error{"EnsureNetwork": errors.New("daemon down")}
	if _, err := l.Attach(ctx, web, domain.Spec{Host: "c.io", Port: 80}, false, nil); err == nil {
		t.Error("attach kept the row without its network")
	}
	if ds, _ := l.ListByTile(ctx, web); len(ds) != 0 {
		t.Errorf("rows = %v", ds)
	}
}
