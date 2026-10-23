package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

const sentinelKey = "k3y-never-shown-0451"

func dnsRig(t *testing.T) (*servicetest.Env, string, string) {
	t.Helper()
	env := servicetest.New(t)
	env.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "shop.io", OurDNS: true}, {Name: "away.com"}}
	return env, env.Org(t, "acme"), env.Org(t, "other")
}

func account() map[string]string {
	return map[string]string{"user": "jo", "key": sentinelKey, "client_ip": "203.0.113.7"}
}

// Adding checks the account and lists its domains; the key is never in
// what comes back, nor in any list.
func TestAddDNSConnector(t *testing.T) {
	env, acme, _ := dnsRig(t)
	ctx := context.Background()
	bad := account()
	bad["key"] = servicetest.BadKey
	if inv, ok := errs.IsInvalid(func() error {
		_, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: bad})
		return err
	}()); !ok || inv.Field != "key" {
		t.Error("a bad key: want an Invalid on key")
	}
	if cs, _ := env.Orch.DNSConnectors(ctx, nil); len(cs) != 0 {
		t.Fatalf("a refused account was saved: %v", cs)
	}
	got, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil || len(got.Domains) != 3 || got.Domains[2].State != service.DNSElsewhere {
		t.Fatalf("add = %+v %v", got, err)
	}
	if _, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()}); err != nil {
		t.Fatal(err)
	}
	srvList, _ := env.Orch.DNSConnectors(ctx, nil)
	orgList, _ := env.Orch.DNSConnectors(ctx, &acme)
	gitList, _ := env.Orch.Connectors(ctx, acme)
	srvGit, _ := env.Orch.ServerConnectors(ctx)
	if len(srvList) != 1 || len(orgList) != 1 || len(gitList) != 0 || len(srvGit) != 0 {
		t.Errorf("lists: server dns %d, org dns %d, org git %d, server git %d; want 1 1 0 0", len(srvList), len(orgList), len(gitList), len(srvGit))
	}
	b, _ := json.Marshal([]any{got, srvList, orgList})
	if strings.Contains(string(b), sentinelKey) {
		t.Error("the API key is in the output")
	}
	ds, err := env.Orch.DNSDomains(ctx, nil, got.Connector.ID)
	if err != nil || len(ds) != 3 {
		t.Errorf("domains = %v %v", ds, err)
	}
	// the stored account still holds the key: the provider is opened with it
	if o := env.DNS.Opened[len(env.DNS.Opened)-1]; o["key"] != sentinelKey || o["client_ip"] != "203.0.113.7" {
		t.Errorf("opened with %q %q", o["user"], o["client_ip"])
	}
}

// An org's DNS connector is that org's: another org, and the server
// routes, never reach it; an org never reaches a server one either. Git
// verbs never touch a DNS connector.
func TestDNSConnectorIsOrgs(t *testing.T) {
	env, acme, other := dnsRig(t)
	ctx := context.Background()
	mine, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: &acme, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	id := mine.Connector.ID
	for name, err := range map[string]error{
		"other org domains": func() error { _, err := env.Orch.DNSDomains(ctx, &other, id); return err }(),
		"server domains":    func() error { _, err := env.Orch.DNSDomains(ctx, nil, id); return err }(),
		"other org manage": func() error {
			_, err := env.Orch.ManageDNSDomain(ctx, &other, id, "acme.dev", nil)
			return err
		}(),
		"other org delete":       env.Orch.RemoveConnector(ctx, &other, id),
		"org reaches server one": func() error { _, err := env.Orch.DNSDomains(ctx, &acme, srv.Connector.ID); return err }(),
		"git rename":             func() error { _, err := env.Orch.RenameConnector(ctx, acme, id, "x"); return err }(),
		"git delete":             env.Orch.DeleteConnector(ctx, acme, id),
	} {
		if !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s = %v, want not found", name, err)
		}
	}
	if _, err := env.Orch.ShareConnector(ctx, srv.Connector.ID, nil, true); err == nil {
		t.Error("a DNS connector was shared like a git one")
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, &acme, id, "acme.dev", []string{other}); err == nil {
		t.Error("an org connector's domain was offered to another org")
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, &acme, id, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.Orch.UsableDNSDomains(ctx, other); len(got) != 0 {
		t.Errorf("other org may use %v", got)
	}
	if got, _ := env.Orch.UsableDNSDomains(ctx, acme); !slices.Equal(got, []string{"acme.dev"}) {
		t.Errorf("acme may use %v", got)
	}
	// one domain, one connector
	if _, ok := errs.IsConflict(func() error {
		_, err := env.Orch.ManageDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", nil)
		return err
	}()); !ok {
		t.Error("a domain managed through two connectors")
	}
}

// A server connector's managed domain is available to every org by
// default, or to the chosen ones only; unmanaged it is nobody's.
func TestDNSDomainAvailableTo(t *testing.T) {
	env, acme, other := dnsRig(t)
	ctx := context.Background()
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	id := srv.Connector.ID
	usable := func(org string) []string {
		got, err := env.Orch.UsableDNSDomains(ctx, org)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, id, "shop.io", nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(usable(acme), []string{"shop.io"}) || !slices.Equal(usable(other), []string{"shop.io"}) {
		t.Errorf("all orgs: acme %v, other %v", usable(acme), usable(other))
	}
	d, err := env.Orch.ManageDNSDomain(ctx, nil, id, "shop.io", []string{acme})
	if err != nil || d.ShareAll || !slices.Equal(d.OrgIDs, []string{acme}) {
		t.Fatalf("share = %+v %v", d, err)
	}
	if !slices.Equal(usable(acme), []string{"shop.io"}) || len(usable(other)) != 0 {
		t.Errorf("acme only: acme %v, other %v", usable(acme), usable(other))
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, id, "shop.io", []string{"nope"}); err == nil {
		t.Error("shared with an org that does not exist")
	}
	if _, ok := errs.IsConflict(func() error { _, err := env.Orch.ManageDNSDomain(ctx, nil, id, "away.com", nil); return err }()); !ok {
		t.Error("DNS elsewhere: want a conflict naming take over")
	}
	ds, _ := env.Orch.DNSDomains(ctx, nil, id)
	if ds[1].Name != "shop.io" || ds[1].State != service.DNSManaged {
		t.Errorf("domains = %+v", ds)
	}
	if err := env.Orch.UnmanageDNSDomain(ctx, nil, id, "shop.io"); err != nil {
		t.Fatal(err)
	}
	if len(usable(acme)) != 0 {
		t.Errorf("unmanaged, still usable: %v", usable(acme))
	}
	// take over then manage
	if err := env.Orch.TakeOverDNSDomain(ctx, nil, id, "away.com", "shop.io"); err == nil || len(env.DNS.TookOver) != 0 {
		t.Error("take over with the wrong domain typed")
	}
	if err := env.Orch.TakeOverDNSDomain(ctx, nil, id, "away.com", "away.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, id, "away.com", nil); err != nil {
		t.Errorf("manage after take over: %v", err)
	}
}
