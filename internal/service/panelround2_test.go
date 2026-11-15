package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/libdns/libdns"
)

// Moving the panel record while a hand-made AAAA sits at the name: stackr's
// old A is never deleted when the new one cannot be written.
func TestPanelMoveKeepsOldRecordNextToForeignSibling(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.5"})
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") {
		t.Fatalf("setup: %v", r)
	}
	env.DNS.Put("acme.dev", libdns.RR{Name: "stackr", Type: "AAAA", Data: "2001:db8::99"})
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.6"})
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") && !has(r, "stackr", "A", "203.0.113.6") {
		t.Errorf("panel name lost its A record: %v", r)
	}
	if n := recordRows(t, env, "stackr"); n != 1 {
		t.Errorf("rows = %d, stackr's row was dropped", n)
	}
}

// The home IP moves; the resolver still answers the old value: no banner.
func TestPanelSeenMoveRaisesNoBanner(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1"}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	n.ip, n.ips = "198.51.100.88", []string{"198.51.100.77"}
	resync(t, env)
	if m := panelProblem(t, env, "panel.address"); m != "" {
		t.Errorf("false banner after own move: %q", m)
	}
}

// Own IPv6 and a seen IPv4 written; then the resolver answers something
// else: the A record is kept (held), not dropped with the AAAA written.
func TestPanelHeldKeepsTheARecord(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "2001:db8::1")
	setSettings(t, env, map[string]string{"ipv6": "true"})
	n := &panelNet{ip: "198.51.100.77", vantage: "w1"}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "198.51.100.77") {
		t.Fatalf("setup: %v", r)
	}
	n.ip, n.ips = "198.51.100.88", []string{"198.51.100.77"}
	resync(t, env) // moves to .88
	resync(t, env) // the resolver still says .77
	r := env.DNS.Zone("acme.dev")
	if !has(r, "docs.stackr", "A", "198.51.100.88") && !has(r, "docs.stackr", "A", "198.51.100.77") {
		t.Errorf("A record deleted while held: %v", r)
	}
	// nothing usable seen at all: the A stays next to the AAAA
	n.ip = ""
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); !slices.ContainsFunc(r, func(x libdns.RR) bool { return x.Name == "docs.stackr" && x.Type == "A" }) {
		t.Errorf("A record deleted with no seen address: %v", r)
	}
	if panelProblem(t, env, "panel.address") == "" {
		t.Error("no warning while the A record's address cannot be told")
	}
}

// A failed lookup keeps the state: it opens no "cannot tell" warning.
func TestPanelFailedLookupOpensNoCannotTell(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	env.Orch.SetPanelSeams(
		func(context.Context, string) (string, string) { return "198.51.100.77", "w1" },
		nil,
		func(context.Context, string) ([]string, error) { return nil, errors.New("SERVFAIL") },
	)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.address"); m != "" {
		t.Errorf("warning on a failed lookup: %q", m)
	}
}

// An open banner stays while the lookup fails: only an answer clears it.
func TestPanelFailedLookupKeepsTheAddressBanner(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1", ips: []string{"192.0.2.9"}}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	if panelProblem(t, env, "panel.address") == "" {
		t.Fatal("setup: no banner for a held name")
	}
	env.Orch.SetPanelSeams(
		func(context.Context, string) (string, string) { return "198.51.100.77", "w1" },
		nil,
		func(context.Context, string) ([]string, error) { return nil, errors.New("SERVFAIL") },
	)
	env.Orch.CheckPanelName(context.Background())
	if panelProblem(t, env, "panel.address") == "" {
		t.Error("a failed lookup cleared the banner")
	}
}

// A private answer (hosts file, split horizon) never holds the record.
func TestPanelPrivateAnswerDoesNotHoldTheRecord(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1", ips: []string{"192.168.1.5"}}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "198.51.100.77") {
		t.Errorf("record held over a private answer: %v", r)
	}
}

// Right after the sync wrote the record, a resolver that still says "no
// address" raises no critical alarm; a later check judges it.
func TestPanelNoResolveAlarmRightAfterAWrite(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1"}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"}) // the sync that writes
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "198.51.100.77") {
		t.Fatalf("setup: %v", r)
	}
	if m := panelProblem(t, env, "panel.resolve"); m != "" {
		t.Errorf("alarm right after the write: %q", m)
	}
	env.Orch.ForcePanelDue()
	env.Orch.RecheckPanelName(context.Background()) // the tick, with no problem open yet
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Error("the tick's recheck stayed silent after a write")
	}
}

// Moving the panel name to another name in the zone: the new record is
// written before the old one goes, so a refused write leaves the old one.
func TestPanelNameMoveWritesBeforeItDeletes(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.1") {
		t.Fatalf("setup: %v", r)
	}
	env.DNS.Fail(errors.New("provider down"))
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.1") {
		t.Errorf("old name lost its record when the new one failed: %v", r)
	}
	env.DNS.Fail(nil)
	resync(t, env)
	r := env.DNS.Zone("acme.dev")
	if !has(r, "docs.stackr", "A", "198.51.100.1") || slices.ContainsFunc(r, func(x libdns.RR) bool { return x.Name == "stackr" }) {
		t.Errorf("after the provider is back: %v", r)
	}
}

// A move replaces only the record it owned: one an operator added at the
// same name and type after stackr read the zone stays.
func TestPanelMoveKeepsRecordAddedMeanwhile(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.5"})
	env.DNS.AfterNextGet(func() {
		env.DNS.Put("acme.dev", libdns.RR{Name: "stackr", Type: "A", Data: "203.0.113.77"})
	})
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.6"})
	r := panelRecords(env)
	if !has(r, "stackr", "A", "203.0.113.6") || !has(r, "stackr", "A", "203.0.113.77") || has(r, "stackr", "A", "203.0.113.5") {
		t.Errorf("zone after the move: %v", r)
	}
}
