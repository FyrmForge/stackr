package service_test

import (
	"context"
	"slices"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// panelNet is the outside world a test's panel sees: the address nodes
// outside see (ip, "" = none), a node to ask from, and the probe's and the
// public DNS's answers.
type panelNet struct {
	ip, vantage string
	who         string
	ips         []string
}

func (n *panelNet) set(env *servicetest.Env) {
	env.Orch.SetPanelSeams(
		func(context.Context, string) (string, string) { return n.ip, n.vantage },
		func(context.Context, string, string) (string, bool) { return n.who, true },
		func(context.Context, string) ([]string, error) { return n.ips, nil },
	)
}

func panelProblem(t *testing.T, env *servicetest.Env, key string) string {
	t.Helper()
	ps, err := env.Orch.Problems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Key == key {
			return p.Message
		}
	}
	return ""
}

func resync(t *testing.T, env *servicetest.Env) {
	t.Helper()
	if err := env.Orch.SyncProxy(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
}

func panelRecords(env *servicetest.Env) []libdns.RR {
	return slices.DeleteFunc(env.DNS.Zone("acme.dev"), func(r libdns.RR) bool { return r.Name != "stackr" })
}

// Behind NAT with a node outside, the panel's record is the address the
// outside sees, and it follows a changed home IP.
func TestPanelRecordFollowsTheOutsideIP(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1"}
	n.set(env)
	resync(t, env)
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.77") {
		t.Fatalf("the outside address is not the record: %v", r)
	}
	n.ip = "198.51.100.88"
	resync(t, env)
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.88") || has(r, "stackr", "A", "198.51.100.77") {
		t.Errorf("the record did not follow the new address: %v", r)
	}
}

// No public address and no node outside: nothing is written, what stackr
// has stays, and a banner asks the admin to choose. Typing an address clears it.
func TestPanelRecordNoOutsideAsksTheAdmin(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	ctx := context.Background()
	setIP(t, env, "")
	n := &panelNet{}
	n.set(env)
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_domain": "docs.stackr.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); slices.ContainsFunc(r, func(r libdns.RR) bool { return r.Name == "docs.stackr" }) {
		t.Errorf("a record was written with no address: %v", r)
	}
	if m := panelProblem(t, env, "panel.address"); m == "" {
		t.Fatal("no banner asking for an address")
	}
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_address": "203.0.113.5"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if v, _ := env.Orch.Setting(ctx, "panel_address"); v != "ip:203.0.113.5" {
		t.Errorf("stored %q, want ip:203.0.113.5", v)
	}
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "203.0.113.5") {
		t.Errorf("the typed address is not the record: %v", r)
	}
	if m := panelProblem(t, env, "panel.address"); m != "" {
		t.Errorf("the banner stayed: %s", m)
	}
}

// ip:<addr> writes that address whatever the node has; self writes
// nothing, keeps what is there and raises no banner.
func TestPanelAddressIPAndSelf(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	ctx := context.Background()
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_address": "ip:203.0.113.5"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") || has(r, "stackr", "A", "198.51.100.1") {
		t.Errorf("ip mode: %v", r)
	}
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_address": "self"}); err != nil {
		t.Fatal(err)
	}
	setIP(t, env, "")
	resync(t, env)
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") {
		t.Errorf("self mode touched the record: %v", r)
	}
	if m := panelProblem(t, env, "panel.address"); m != "" {
		t.Errorf("self mode banner: %s", m)
	}
	if err := env.Orch.SetSettings(ctx, map[string]string{"panel_address": "not-an-address"}); err == nil {
		t.Error("a bad panel address was saved")
	}
}

// The per-node address is gone: a node row's old value is never written,
// and the API's node carries no address.
func TestNodeAddressIsGone(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	ctx := context.Background()
	ns, _ := env.Store.Nodes.List(ctx)
	ns[0].Address = "n1.stackr.acme.dev"
	if err := env.Store.Nodes.Update(ctx, ns[0]); err != nil {
		t.Fatal(err)
	}
	resync(t, env)
	if z := env.DNS.Zone("acme.dev"); slices.ContainsFunc(z, func(r libdns.RR) bool { return r.Name == "n1.stackr" }) {
		t.Errorf("a node address got a record: %v", z)
	}
}

// The name check: a name that does not resolve to the server, or that the
// outside cannot reach, raises its banner; each clears when fixed.
func TestPanelNameChecks(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	n := &panelNet{ips: nil, vantage: "w1", who: "nobody"}
	n.set(env)
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.resolve"); m != "stackr.acme.dev does not resolve to this server." {
		t.Errorf("resolve banner = %q", m)
	}
	if m := panelProblem(t, env, "panel.reach"); m != "" {
		t.Errorf("reach banner while the name does not resolve: %q", m)
	}
	n.ips = []string{"198.51.100.1"}
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.resolve"); m != "" {
		t.Errorf("resolve banner stayed: %q", m)
	}
	if m := panelProblem(t, env, "panel.reach"); m == "" {
		t.Error("no reach banner while nothing answers")
	}
	ns, _ := env.Store.Nodes.List(context.Background())
	n.who = ns[0].ID
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.reach"); m != "" {
		t.Errorf("reach banner stayed: %q", m)
	}
	// self skips the resolve comparison but keeps the reach probe
	if err := env.Orch.SetSettings(context.Background(), map[string]string{"panel_address": "self"}); err != nil {
		t.Fatal(err)
	}
	n.ips, n.who = []string{"192.0.2.9"}, "nobody"
	env.Orch.CheckPanelName(context.Background())
	if panelProblem(t, env, "panel.resolve") != "" || panelProblem(t, env, "panel.reach") == "" {
		t.Error("self: want no resolve banner and a reach banner")
	}
}
