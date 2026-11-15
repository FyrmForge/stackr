package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// H1: a second sync inside the TTL of a just-written panel record judges
// nothing: the resolver's still-cached old answer raises no alarm and the
// TTL recheck stays due.
func TestR6SecondSyncJudgesInsideTTL(t *testing.T) {
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1", who: "", ips: []string{"198.51.100.77"}}
	n.set(env)
	resync(t, env)
	env.Orch.ForcePanelDue()
	env.Orch.RecheckPanelName(context.Background())
	if p := panelProblem(t, env, "panel.resolve"); p != "" {
		t.Fatalf("setup: %s", p)
	}
	n.ip = "198.51.100.88" // the home IP changed; resolvers still cache .77
	resync(t, env)
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.88") {
		t.Fatalf("setup: record did not move: %v", r)
	}
	if p := panelProblem(t, env, "panel.resolve"); p != "" {
		t.Fatalf("the writing sync itself alarmed: %s", p)
	}
	resync(t, env) // any deploy / kick seconds later
	if p := panelProblem(t, env, "panel.resolve"); p != "" {
		t.Errorf("alarm on a cached answer inside the TTL: %s", p)
	}
	if !env.Orch.PanelDue() {
		t.Errorf("the TTL recheck was cleared by the second sync")
	}
}

// H1: a tick inside the TTL of a just-written panel record, with an alert
// open, judges nothing: the alert stays and the recheck stays due.
func TestR6TickInsideTTLChangesNothing(t *testing.T) {
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1", who: "", ips: []string{"203.0.113.5"}}
	n.set(env)
	resync(t, env)
	env.Orch.ForcePanelDue()
	env.Orch.RecheckPanelName(context.Background())
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Fatal("setup: no resolve alert opened")
	}
	n.ip, n.ips = "198.51.100.88", []string{"198.51.100.88"}
	resync(t, env) // writes: the TTL starts
	env.Orch.RecheckPanelName(context.Background())
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Errorf("a tick inside the TTL cleared the alert")
	}
	if !env.Orch.PanelDue() {
		t.Errorf("a tick inside the TTL cleared the recheck")
	}
}

// H3: provider without Replace; "Replace it" on a name whose foreign record
// already equals the wanted one must not leave the name bare.
func TestR6FallbackReplaceIdentical(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	env.DNS.NoReplace()
	ctx := context.Background()
	env.DNS.Put("acme.dev", a("shop", "198.51.100.1"))
	if _, err := env.Orch.AttachDomainReplace(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}, true); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	resync(t, env)
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.1") {
		t.Errorf("shop has no record: %v", z)
	}
}

// H2: provider without Replace; a move whose Append lands and whose Delete
// fails: the next sync removes the old value.
func TestR6FallbackMoveDeleteFails(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	env.DNS.NoReplace()
	ctx := context.Background()
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	setIP(t, env, "198.51.100.2")
	env.DNS.FailAfter(1, errProviderDown)
	_ = env.Orch.SyncProxy(ctx)
	env.Orch.WaitDNS()
	env.DNS.Fail(nil)
	resync(t, env)
	resync(t, env)
	if z := env.DNS.Zone("acme.dev"); has(z, "shop", "A", "198.51.100.1") || !has(z, "shop", "A", "198.51.100.2") {
		t.Errorf("old value orphaned: %v", z)
	}
}

// H4: the provider applied a move but its answer was lost: the next sync
// takes the live record over, no further write and no "taken".
func TestR6LostAnswerMoveHeals(t *testing.T) {
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: "shop.acme.dev"}); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
	setIP(t, env, "198.51.100.2")
	env.DNS.LoseNextAnswer(errProviderDown)
	_ = env.Orch.SyncProxy(ctx)
	env.Orch.WaitDNS()
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") {
		t.Fatalf("setup: the write did not land: %v", z)
	}
	resync(t, env)
	if e := env.Orch.DNSSyncError(); e != "" {
		t.Errorf("next sync: %s", e)
	}
	_, del, _ := env.DNS.Log()
	before := len(del)
	resync(t, env)
	if _, del, _ = env.DNS.Log(); len(del) != before {
		t.Errorf("a healed zone was written again: %v", del)
	}
	if z := env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") || has(z, "shop", "A", "198.51.100.1") {
		t.Errorf("zone: %v", z)
	}
}
