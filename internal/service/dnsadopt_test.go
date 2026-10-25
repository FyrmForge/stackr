package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// adoptRig is acme.dev on a server connector, not managed yet, with two
// tile domains under it (shop and dev) and a zone that already holds
// records: two pointing at our node (shop, which a tile serves, and www,
// which none does), a wildcard at our node, and one pointing elsewhere.
func adoptRig(t *testing.T) (env *servicetest.Env, connector string) {
	t.Helper()
	env, acme, _ := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	tl := env.Tile(t, acme)
	for _, h := range []string{"shop.acme.dev", "dev.acme.dev"} {
		if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	env.DNS.Put("acme.dev", a("shop", "198.51.100.1"), a("www", "198.51.100.1"), a("*", "198.51.100.1"),
		a("old", "203.0.113.9"), libdns.RR{Name: "shop", Type: "TXT", Data: "keep"})
	return env, srv.Connector.ID
}

// The confirm lists the records stackr writes for hosts tiles use, and the
// existing ones at our nodes: ticked when a tile serves the name. Wildcards
// and records pointing elsewhere are never listed.
func TestManagePlanListsWritesAndAdoptable(t *testing.T) {
	env, id := adoptRig(t)
	plan, err := env.Orch.PlanManageDNSDomain(context.Background(), nil, id, "acme.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Writes) != 1 || plan.Writes[0] != (service.DNSWrite{Host: "dev.acme.dev", Type: "A", Value: "198.51.100.1"}) {
		t.Errorf("writes = %+v", plan.Writes)
	}
	if len(plan.Adopt) != 2 {
		t.Fatalf("adopt = %+v", plan.Adopt)
	}
	for _, r := range plan.Adopt {
		switch r.Name {
		case "shop":
			if !r.Serves || r.Host != "shop.acme.dev" {
				t.Errorf("shop = %+v", r)
			}
		case "www":
			if r.Serves {
				t.Errorf("www is served by no tile: %+v", r)
			}
		default:
			t.Errorf("listed %s, a wildcard or a record pointing elsewhere", r.Name)
		}
	}
}

// An empty plan marshals both lists as [], never null: the API schema requires arrays.
func TestManagePlanEmptyListsAreArrays(t *testing.T) {
	env, _, _ := dnsRig(t)
	setIP(t, env, "198.51.100.1")
	srv, err := env.Orch.AddConnector(context.Background(), service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := env.Orch.PlanManageDNSDomain(context.Background(), nil, srv.Connector.ID, "acme.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plan)
	if string(b) != `{"writes":[],"adopt":[]}` {
		t.Errorf("plan = %s", b)
	}
}

// Adopted records stay in the zone through a detach (reconcile) and through
// stop managing; only the rows go. A ref the plan did not list is ignored.
func TestAdoptedRecordsAreNeverDeleted(t *testing.T) {
	env, id := adoptRig(t)
	ctx := context.Background()
	adopt := []service.DNSRef{
		{Name: "shop", Type: "A", Value: "198.51.100.1"},
		{Name: "www", Type: "A", Value: "198.51.100.1"},
		{Name: "old", Type: "A", Value: "203.0.113.9"}, // not ours to take
	}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, id, "acme.dev", nil, adopt); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	rows, _ := env.Store.DNSRecords.List(ctx)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Name] = r.Adopted
	}
	if !got["shop"] || !got["www"] || got["old"] {
		t.Fatalf("rows = %+v", rows)
	}
	if adopted, ok := got["dev"]; !ok || adopted {
		t.Errorf("dev is stackr's own write, not adopted: %+v", rows)
	}
	if _, ok := got["old"]; ok {
		t.Errorf("a record that was not listed was adopted")
	}

	// the tile domain goes: stackr's own dev record goes with it, the adopted
	// ones stay
	for _, host := range []string{"shop.acme.dev", "dev.acme.dev"} {
		detach(t, env, host)
	}
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	if !has(z, "shop", "A", "198.51.100.1") || !has(z, "www", "A", "198.51.100.1") || has(z, "dev", "A", "198.51.100.1") {
		t.Fatalf("after detach: %v", z)
	}

	if err := env.Orch.UnmanageDNSDomain(ctx, nil, id, "acme.dev"); err != nil {
		t.Fatal(err)
	}
	z = env.DNS.Zone("acme.dev")
	if !has(z, "shop", "A", "198.51.100.1") || !has(z, "www", "A", "198.51.100.1") || !has(z, "old", "A", "203.0.113.9") {
		t.Errorf("after stop managing: %v", z)
	}
	if rows, _ := env.Store.DNSRecords.List(ctx); len(rows) != 0 {
		t.Errorf("rows after stop managing: %+v", rows)
	}
}

func detach(t *testing.T, env *servicetest.Env, host string) {
	t.Helper()
	ctx := context.Background()
	rows, _ := env.Store.Domains.List(ctx)
	for _, d := range rows {
		if d.Host == host {
			if err := env.Orch.DetachDomain(ctx, d.ID); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no domain %s", host)
}

// An adopted record follows its host's ingress node: it is repointed when
// the address moves.
func TestAdoptedRecordRepoints(t *testing.T) {
	env, id := adoptRig(t)
	ctx := context.Background()
	adopt := []service.DNSRef{{Name: "shop", Type: "A", Value: "198.51.100.1"}}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, id, "acme.dev", nil, adopt); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	setIP(t, env, "198.51.100.2")
	if err := env.Orch.SyncProxy(ctx); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") || has(z, "shop", "A", "198.51.100.1") {
		t.Fatalf("after the move: %v", z)
	}
	rows, _ := env.Store.DNSRecords.List(ctx)
	for _, r := range rows {
		if r.Name == "shop" && !r.Adopted {
			t.Errorf("repointing dropped the adopted flag: %+v", r)
		}
	}
}

// A tile domain added later onto a record that points at our node asks to
// adopt it, one row; a record pointing elsewhere keeps the Replace question.
func TestAdoptHostOnLaterDomain(t *testing.T) {
	env, acme, _, tl := recordsRig(t)
	ctx := context.Background()
	env.DNS.Put("acme.dev", a("shop", "198.51.100.1"), a("blog", "203.0.113.9"))

	var taken *service.RecordTaken
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); !errors.As(err, &taken) {
		t.Fatalf("attach over an existing record = %v", err)
	}
	rs, err := env.Orch.PlanAdoptDNSHost(ctx, acme, "shop.acme.dev")
	if err != nil || len(rs) != 1 || rs[0].Value != "198.51.100.1" {
		t.Fatalf("plan = %+v, %v", rs, err)
	}
	if rs, _ := env.Orch.PlanAdoptDNSHost(ctx, acme, "blog.acme.dev"); len(rs) != 0 {
		t.Errorf("a record pointing elsewhere was offered: %+v", rs)
	}
	if err := env.Orch.AdoptDNSHost(ctx, acme, "shop.acme.dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatalf("attach after adopting: %v", err)
	}
	rows, _ := env.Store.DNSRecords.List(ctx)
	if len(rows) != 1 || !rows[0].Adopted {
		t.Errorf("rows = %+v", rows)
	}
}
