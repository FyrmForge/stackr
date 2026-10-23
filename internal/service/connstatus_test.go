package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Updating credentials: the form shows no secret; an empty secret keeps
// the stored one; a key the provider refuses is refused and the old
// settings stay; the status says working, then the refusal a check got.
func TestUpdateConnectorCredentials(t *testing.T) {
	env, acme, _ := dnsRig(t)
	ctx := context.Background()
	a, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	id := a.Connector.ID
	fs, vals, err := env.Orch.ConnectorCredentials(ctx, &acme, id)
	if err != nil || len(fs) != 3 || vals["user"] != "jo" || vals["key"] != "" {
		t.Fatalf("form = %v, user %q, key shown %v, %v", len(fs), vals["user"], vals["key"] != "", err)
	}
	c, err := env.Orch.UpdateConnector(ctx, &acme, id, map[string]string{"user": "ann", "key": "", "client_ip": "203.0.113.8"})
	if err != nil || c.Name != "Namecheap · ann" || c.CheckError != "" || c.CheckedAt == nil {
		t.Fatalf("update = %+v %v", c, err)
	}
	if o := env.DNS.Opened[len(env.DNS.Opened)-1]; o["key"] != sentinelKey || o["user"] != "ann" {
		t.Errorf("the empty key was not kept (user %q)", o["user"])
	}
	_, err = env.Orch.UpdateConnector(ctx, &acme, id, map[string]string{"user": "bob", "key": servicetest.BadKey, "client_ip": "203.0.113.8"})
	if inv, ok := errs.IsInvalid(err); !ok || inv.Field != "key" {
		t.Fatalf("a refused key = %v", err)
	}
	if _, vals, _ := env.Orch.ConnectorCredentials(ctx, &acme, id); vals["user"] != "ann" {
		t.Errorf("a refused update changed the settings: user %q", vals["user"])
	}
	row, _ := env.Store.Connectors.Get(ctx, id)
	if !strings.Contains(row.Settings, sentinelKey) {
		t.Error("a refused update lost the stored key")
	}
	// a check that fails says why, from the kind
	row.Settings = strings.Replace(row.Settings, sentinelKey, servicetest.BadKey, 1)
	if err := env.Store.Connectors.SetSettings(ctx, id, row.Settings); err != nil {
		t.Fatal(err)
	}
	if c, err := env.Orch.CheckConnector(ctx, &acme, id); err != nil || c.CheckError != "The provider refused the API key" {
		t.Errorf("check = %q %v", c.CheckError, err)
	}
	if _, err := env.Orch.ConnectorStatus(ctx, nil, id); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the server reached an org's connector: %v", err)
	}
}

// The Records tab: the record stackr made for a tile domain, with the
// tile's path and the ingress node it points at; the Used by tab names
// the same tile; stopping managing the domain is refused, naming it.
func TestConnectorRecordsAndUsages(t *testing.T) {
	env, acme, other, tl := recordsRig(t)
	ctx := context.Background()
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	cs, _ := env.Orch.DNSConnectors(ctx, nil)
	id := cs[0].ID
	rs, err := env.Orch.ConnectorRecords(ctx, nil, id)
	if err != nil || len(rs) != 1 {
		t.Fatalf("records = %+v %v", rs, err)
	}
	ns, _ := env.Store.Nodes.List(ctx)
	r := rs[0]
	if r.Domain != "acme.dev" || r.Name != "shop" || r.Type != "A" || r.Value != "198.51.100.1" || r.Host != "shop.acme.dev" ||
		r.Usage == nil || r.Usage.Level != service.ScopeTile || r.Usage.ID != tl.ID || r.Node != ns[0].Name {
		t.Fatalf("record = %+v usage %+v", r, r.Usage)
	}
	us, err := env.Orch.ConnectorUsages(ctx, nil, id)
	if err != nil || len(us) != 1 || us[0].ID != tl.ID || us[0].What != "A record shop.acme.dev" || strings.Count(us[0].Path, " / ") != 3 {
		t.Fatalf("usages = %+v %v", us, err)
	}
	if us, _ := env.Orch.ConnectorUsages(ctx, &other, id); len(us) != 0 {
		t.Errorf("another org sees acme's usages: %+v", us)
	}
	if us, _ := env.Orch.ConnectorUsages(ctx, &acme, id); len(us) != 1 {
		t.Errorf("acme does not see its own: %+v", us)
	}
	err = env.Orch.UnmanageDNSDomain(ctx, nil, id, "acme.dev")
	var in *service.InUse
	if !errors.As(err, &in) || len(in.Usages) != 1 || in.Usages[0].ID != tl.ID {
		t.Errorf("unmanage in use = %v", err)
	}
}

// Git usages: a stack whose config repo clones through a shared server
// connection; an org sees only its own stacks.
func TestGitUsages(t *testing.T) {
	env, ctx := servicetest.New(t), context.Background()
	acme, globex := env.Org(t, "acme"), env.Org(t, "globex")
	srv := env.ServerConnector(t, "gh", "s")
	if _, err := env.Orch.ShareConnector(ctx, srv, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, og := range []string{acme, globex} {
		if _, err := env.Orch.SetConfigRepo(ctx, env.Stack(t, og, "shop").Stack, srv, "https://github.com/x/shop", "main", ""); err != nil {
			t.Fatal(err)
		}
	}
	us, err := env.Orch.ConnectorUsages(ctx, nil, srv)
	if err != nil || len(us) != 2 || us[0].Level != service.ScopeStack || us[0].What != "clones x/shop@main" {
		t.Fatalf("admin usages = %+v %v", us, err)
	}
	us, _ = env.Orch.ConnectorUsages(ctx, &acme, srv)
	if len(us) != 1 || us[0].OrgID != acme || !strings.HasPrefix(us[0].Path, "acme / shop") {
		t.Errorf("acme's usages = %+v", us)
	}
}
