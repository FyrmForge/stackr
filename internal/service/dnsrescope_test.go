package service_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// rescopeRig is acme.dev managed only under test, with a tile serving
// app.test.acme.dev (inside) and shop.acme.dev (outside, typed by hand),
// and www.acme.dev already pointing at our node.
func rescopeRig(t *testing.T) (env *servicetest.Env, connector string, tl servicetest.Tile) {
	t.Helper()
	env, acme, _ := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	env.DNS.Put("acme.dev", a("www", "198.51.100.1"), a("old", "203.0.113.9"))
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, srv.Connector.ID, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	tl = env.Tile(t, acme)
	for _, h := range []string{"app.test.acme.dev", "shop.acme.dev"} {
		if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	env.Orch.WaitDNS()
	return env, srv.Connector.ID, tl
}

func resourceHosts(t *testing.T, env *servicetest.Env) []string {
	t.Helper()
	rs, err := env.Orch.AllDomainResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Host)
	}
	slices.Sort(out)
	return out
}

func recordNames(t *testing.T, env *servicetest.Env) map[string]bool {
	t.Helper()
	rows, err := env.Store.DNSRecords.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[r.Name] = r.Adopted
	}
	return out
}

// Widening offers the records now inside that point at our nodes and
// writes the hosts tiles use; narrowing hands stackr's records outside
// back: they stay in the zone, their rows go, a later sync leaves them.
// The domain resource stackr made follows the scope both ways.
func TestRescopeWidenThenNarrow(t *testing.T) {
	t.Parallel()
	env, id, _ := rescopeRig(t)
	ctx := context.Background()
	if got := recordNames(t, env); len(got) != 1 || got["app.test"] {
		t.Fatalf("rows before = %v", got)
	}
	if got := resourceHosts(t, env); !slices.Contains(got, "test.acme.dev") {
		t.Fatalf("resources = %v", got)
	}

	plan, err := env.Orch.PlanRescopeDNSDomain(ctx, nil, id, "acme.dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Writes) != 1 || plan.Writes[0].Host != "shop.acme.dev" {
		t.Errorf("writes = %+v", plan.Writes)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].Name != "www" || plan.Adopt[0].Serves {
		t.Errorf("adopt = %+v", plan.Adopt)
	}
	if len(plan.HandBack) != 0 || plan.Resource != "acme.dev" {
		t.Errorf("widen hand back = %v, resource = %q", plan.HandBack, plan.Resource)
	}

	d, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "", nil, []service.DNSRef{plan.Adopt[0].DNSRef})
	if err != nil || d.Scope != "" {
		t.Fatalf("widen = %+v %v", d, err)
	}
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.1") || !has(z, "old", "A", "203.0.113.9") {
		t.Errorf("zone after widen: %v", z)
	}
	if got := recordNames(t, env); !got["www"] || len(got) != 3 {
		t.Errorf("rows after widen = %v", got)
	}
	if got := resourceHosts(t, env); !slices.Contains(got, "acme.dev") || slices.Contains(got, "test.acme.dev") {
		t.Errorf("resources after widen = %v", got)
	}

	plan, err = env.Orch.PlanRescopeDNSDomain(ctx, nil, id, "acme.dev", "test.acme.dev")
	if err != nil {
		t.Fatal(err)
	}
	var back []string
	for _, w := range plan.HandBack {
		back = append(back, w.Host)
	}
	slices.Sort(back)
	if !slices.Equal(back, []string{"shop.acme.dev", "www.acme.dev"}) {
		t.Errorf("hand back = %+v", plan.HandBack)
	}
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if got := recordNames(t, env); len(got) != 1 {
		t.Errorf("rows after narrow = %v", got)
	}
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.1") || !has(z, "www", "A", "198.51.100.1") || !has(z, "app.test", "A", "198.51.100.1") {
		t.Errorf("zone after narrow: %v", z)
	}
	if got := resourceHosts(t, env); !slices.Contains(got, "test.acme.dev") || slices.Contains(got, "acme.dev") {
		t.Errorf("resources after narrow = %v", got)
	}
	if e := env.Orch.DNSSyncError(); e != "" {
		t.Errorf("the sync after narrowing: %s", e)
	}

	// the same scope again changes nothing
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Errorf("same scope: %v", err)
	}
}

// A narrowing that would leave a generated hostname outside the scope is
// refused, naming it.
func TestRescopeRefusesGeneratedHosts(t *testing.T) {
	t.Parallel()
	env, id, tl := rescopeRig(t)
	ctx := context.Background()
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	d, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(d.Host, ".acme.dev") || strings.HasSuffix(d.Host, ".test.acme.dev") {
		t.Skipf("auto host %s does not sit under the managed resource", d.Host)
	}
	for _, under := range []string{"test.acme.dev", "acme.dev"} {
		_, err := env.Orch.PlanRescopeDNSDomain(ctx, nil, id, "acme.dev", under)
		if under == "acme.dev" {
			if err != nil {
				t.Errorf("same scope with a generated host: %v", err)
			}
			continue
		}
		if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), d.Host) {
			t.Errorf("narrow over %s = %v, want a conflict naming it", d.Host, err)
		}
	}
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "test.acme.dev", nil, nil); err == nil {
		t.Error("a narrowing that strands a generated host went through")
	}
}

// A resource that existed before the domain was managed is never renamed:
// a new one is made at the new scope, as managing does.
func TestRescopeLeavesExistingResource(t *testing.T) {
	t.Parallel()
	env, _, _ := dnsRig(t)
	ctx := context.Background()
	setIP(t, env, "198.51.100.1")
	if _, err := env.Orch.CreateDomainResource(ctx, "instance", "", "test.acme.dev", false, ""); err != nil {
		t.Fatal(err)
	}
	srv, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomainAdopting(ctx, nil, srv.Connector.ID, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := resourceHosts(t, env); !slices.Equal(got, []string{"acme.dev", "test.acme.dev"}) {
		t.Errorf("resources = %v", got)
	}
	// back again: the made one is dropped for the one already there
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, srv.Connector.ID, "acme.dev", "test.acme.dev", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := resourceHosts(t, env); !slices.Equal(got, []string{"test.acme.dev"}) {
		t.Errorf("resources after narrowing back = %v", got)
	}
}

// A confirm planned before another rescope landed is refused: the plan it
// showed is stale. Rescopes of one domain run one at a time, so of many
// confirms of one plan exactly one goes.
func TestRescopeStaleConfirm(t *testing.T) {
	t.Parallel()
	env, id, _ := rescopeRig(t)
	ctx := context.Background()
	plan, err := env.Orch.PlanRescopeDNSDomain(ctx, nil, id, "acme.dev", "")
	if err != nil || plan.From != "test.acme.dev" {
		t.Fatalf("plan = %+v %v", plan, err)
	}
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	stale := func(err error) bool {
		c, ok := errs.IsConflict(err)
		return ok && strings.Contains(c.Msg, "Review the plan again")
	}
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "", &plan.From, nil); !stale(err) {
		t.Fatalf("stale confirm = %v", err)
	}

	from := ""
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "test.acme.dev", &from, nil)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	went := 0
	for err := range results {
		switch {
		case err == nil:
			went++
		case !stale(err):
			t.Errorf("concurrent confirm = %v", err)
		}
	}
	if went != 1 {
		t.Errorf("%d confirms went, want 1", went)
	}
	if got := resourceHosts(t, env); !slices.Equal(got, []string{"test.acme.dev"}) {
		t.Errorf("resources = %v", got)
	}
	// from is compared as a scope, however it was typed: the domain itself
	// is the whole domain
	for _, step := range []struct{ from, to string }{{"Test.Acme.dev.", ""}, {"acme.dev", "test.acme.dev"}} {
		if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", step.to, &step.from, nil); err != nil {
			t.Errorf("from %q: %v", step.from, err)
		}
	}
}

// A narrowing keeps the domain's own custom records outside the new scope
// declared: a widening writes them again.
func TestRescopeKeepsCustomRecordsOutside(t *testing.T) {
	t.Parallel()
	env, id, _ := rescopeRig(t)
	ctx := context.Background()
	if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.AddDNSDomainRecord(ctx, nil, id, "acme.dev", service.CustomRecordIn{Name: "verify", Type: "TXT", Value: "v=1"}); err != nil {
		t.Fatal(err)
	}
	written := func() (bool, bool) {
		t.Helper()
		rs, err := env.Orch.DNSDomainRecords(ctx, nil, id, "acme.dev")
		if err != nil {
			t.Fatal(err)
		}
		if len(rs.Records) != 1 {
			return false, false
		}
		return true, rs.Records[0].Written
	}
	if declared, ok := written(); !declared || !ok {
		t.Fatalf("after add: declared %v written %v", declared, ok)
	}
	for _, scope := range []string{"test.acme.dev", ""} {
		if _, err := env.Orch.RescopeDNSDomain(ctx, nil, id, "acme.dev", scope, nil, nil); err != nil {
			t.Fatal(err)
		}
		env.Orch.WaitDNS()
		declared, ok := written()
		if !declared {
			t.Fatalf("scope %q dropped the declaration", scope)
		}
		if ok != (scope == "") {
			t.Errorf("scope %q: written %v", scope, ok)
		}
	}
	if _, ok := recordNames(t, env)["verify"]; !ok || !has(env.DNS.Zone("acme.dev"), "verify", "TXT", "v=1") {
		t.Errorf("verify after widening: row %v, zone %v", ok, env.DNS.Zone("acme.dev"))
	}
}
