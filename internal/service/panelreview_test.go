package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func setSettings(t *testing.T, env *servicetest.Env, kv map[string]string) {
	t.Helper()
	if err := env.Orch.SetSettings(context.Background(), kv); err != nil {
		t.Fatal(err)
	}
	env.Orch.WaitDNS()
}

func recordRows(t *testing.T, env *servicetest.Env, name string) int {
	t.Helper()
	rows, err := env.Store.DNSRecords.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.Name == name {
			n++
		}
	}
	return n
}

// D1: a moved panel record is one Set; a provider that refuses the write
// never leaves the name without a record.
func TestPanelMoveNeverLeavesTheNameEmpty(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.5"})
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") {
		t.Fatalf("setup: %v", r)
	}
	env.DNS.Fail(errors.New("provider down"))
	setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.6"})
	if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") {
		t.Errorf("the panel name lost its record: %v", r)
	}
	env.DNS.Fail(nil)
	resync(t, env)
	if r := panelRecords(env); len(r) != 1 || !has(r, "stackr", "A", "203.0.113.6") {
		t.Errorf("after the provider is back: %v", r)
	}
}

// D2: the seen address is not written over a name that already points
// elsewhere; the admin is asked. A name that resolves nowhere, or to what
// stackr wrote, still follows.
func TestPanelSeenAddressHeldOverAnotherAnswer(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	n := &panelNet{ip: "198.51.100.77", vantage: "w1", ips: []string{"192.0.2.9"}}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); slices.ContainsFunc(r, func(r libdns.RR) bool { return r.Name == "docs.stackr" }) {
		t.Fatalf("written over a name pointing elsewhere: %v", r)
	}
	m := panelProblem(t, env, "panel.address")
	if !strings.Contains(m, "points at 192.0.2.9") || !strings.Contains(m, "sees 198.51.100.77") {
		t.Errorf("banner = %q", m)
	}
	n.ips = nil
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "198.51.100.77") {
		t.Errorf("not written once the name resolves nowhere: %v", r)
	}
	if m := panelProblem(t, env, "panel.address"); m != "" {
		t.Errorf("banner stayed: %q", m)
	}
	n.ip, n.ips = "198.51.100.88", []string{"198.51.100.77"} // the home IP moved; the name shows stackr's own value
	resync(t, env)
	if r := env.DNS.Zone("acme.dev"); !has(r, "docs.stackr", "A", "198.51.100.88") {
		t.Errorf("did not follow the moved address: %v", r)
	}
}

// D3: a name with no address at all alarms in self mode and with nothing to
// point at: the original outage.
func TestPanelNoAddressAlarmsInSelfAndAuto(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	n := &panelNet{ips: nil}
	n.set(env)
	setSettings(t, env, map[string]string{"panel_address": "self"})
	env.Orch.CheckPanelName(context.Background())
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Error("self: no alarm for a name with no address")
	}
	n.ips = []string{"192.0.2.9"}
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.resolve"); m != "" {
		t.Errorf("self: alarm for a name that resolves: %q", m)
	}
	setIP(t, env, "")
	n.ip, n.ips = "", nil
	setSettings(t, env, map[string]string{"panel_address": "auto"})
	env.Orch.CheckPanelName(context.Background())
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Error("auto, nothing to point at: no alarm for a name with no address")
	}
}

// D4: a hand-made record just like the want is taken over; a different one
// is left alone with no not-in-sync banner.
func TestPanelRecordAlreadyThere(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	(&panelNet{ips: []string{"198.51.100.1"}}).set(env)
	env.DNS.Put("acme.dev", a("docs.stackr", "198.51.100.1"), a("api.stackr", "203.0.113.9"))
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	if recordRows(t, env, "docs.stackr") != 1 {
		t.Error("the identical record was not taken over")
	}
	if e := env.Orch.DNSSyncError(); e != "" {
		t.Errorf("identical record: sync error %q", e)
	}
	setSettings(t, env, map[string]string{"panel_domain": "api.stackr.acme.dev"})
	resync(t, env)
	if e := env.Orch.DNSSyncError(); e != "" || panelProblem(t, env, "dns.records") != "" {
		t.Errorf("a different record: sync error %q", e)
	}
	if r := env.DNS.Zone("acme.dev"); !has(r, "api.stackr", "A", "203.0.113.9") || recordRows(t, env, "api.stackr") != 0 {
		t.Errorf("a record that is not stackr's was touched: %v", r)
	}
}

// D5: an address that is not public (carrier-grade NAT, link-local) is not
// the seen address.
func TestPanelSeenAddressMustBePublic(t *testing.T) {
	t.Parallel()
	for _, ip := range []string{"100.64.1.2", "169.254.1.1", "10.1.2.3"} {
		env, _, _ := scopedRig(t)
		setIP(t, env, "")
		(&panelNet{ip: ip, vantage: "w1"}).set(env)
		setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
		resync(t, env)
		if r := env.DNS.Zone("acme.dev"); slices.ContainsFunc(r, func(r libdns.RR) bool { return r.Name == "docs.stackr" }) {
			t.Errorf("%s was written: %v", ip, r)
		}
		if panelProblem(t, env, "panel.address") == "" {
			t.Errorf("%s: no banner", ip)
		}
	}
}

// D6: a node with only an IPv6 address gets its A from the seen address.
func TestPanelSeenFillsMissingIPv4(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "2001:db8::1")
	setSettings(t, env, map[string]string{"ipv6": "true"})
	(&panelNet{ip: "198.51.100.77", vantage: "w1"}).set(env)
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	resync(t, env)
	r := env.DNS.Zone("acme.dev")
	if !has(r, "docs.stackr", "A", "198.51.100.77") || !has(r, "docs.stackr", "AAAA", "2001:db8::1") {
		t.Errorf("records = %v", r)
	}
}

// D7: a lookup that fails keeps the alert as it is; only an answer opens or
// clears it.
func TestPanelLookupErrorKeepsTheAlert(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	var fail atomic.Bool
	var ips atomic.Value
	ips.Store([]string(nil))
	env.Orch.SetPanelSeams(nil, nil, func(context.Context, string) ([]string, error) {
		if fail.Load() {
			return nil, errors.New("SERVFAIL")
		}
		return ips.Load().([]string), nil
	})
	ctx := context.Background()
	fail.Store(true)
	env.Orch.CheckPanelName(ctx)
	if panelProblem(t, env, "panel.resolve") != "" {
		t.Error("a failed lookup opened the alert")
	}
	fail.Store(false)
	env.Orch.CheckPanelName(ctx)
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Fatal("no alert for a name with no address")
	}
	fail.Store(true)
	env.Orch.CheckPanelName(ctx)
	if panelProblem(t, env, "panel.resolve") == "" {
		t.Error("a failed lookup cleared the alert")
	}
	fail.Store(false)
	ips.Store([]string{"198.51.100.1"})
	env.Orch.CheckPanelName(ctx)
	if panelProblem(t, env, "panel.resolve") != "" {
		t.Error("the alert stayed after a good answer")
	}
}

// D8: private and loopback answers say nothing: no alarm.
func TestPanelPrivateAnswerIsNoAlarm(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	n := &panelNet{ips: []string{"192.168.1.5", "127.0.0.1"}}
	n.set(env)
	env.Orch.CheckPanelName(context.Background())
	if m := panelProblem(t, env, "panel.resolve"); m != "" {
		t.Errorf("alarm for a private answer: %q", m)
	}
}

// D10: one sync asks the mesh once.
func TestPanelSyncAsksTheOutsideOnce(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	var calls atomic.Int32
	env.Orch.SetPanelSeams(
		func(context.Context, string) (string, string) { calls.Add(1); return "198.51.100.77", "w1" },
		func(context.Context, string, string) (string, bool) { return "", false },
		func(context.Context, string) ([]string, error) { return []string{"198.51.100.77"}, nil },
	)
	calls.Store(0)
	resync(t, env)
	if n := calls.Load(); n != 1 {
		t.Errorf("the mesh was asked %d times in one sync, want 1", n)
	}
}

// D11: once stackr has a record for the panel name, a wildcard no longer
// makes the wizard warn.
func TestPanelWildcardWarningEndsWithItsRecord(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	env.Orch.SetPanelSeams(nil, nil, func(context.Context, string) ([]string, error) { return []string{"203.0.113.9"}, nil })
	ctx := context.Background()
	setSettings(t, env, map[string]string{"panel_address": "self", "panel_domain": "docs.stackr.acme.dev"})
	if pa, err := env.Orch.PanelAddress(ctx); err != nil || !pa.Wildcard {
		t.Fatalf("self, no record: %+v %v", pa, err)
	}
	setSettings(t, env, map[string]string{"panel_address": "auto"})
	if pa, err := env.Orch.PanelAddress(ctx); err != nil || pa.Wildcard {
		t.Errorf("record written: %+v %v", pa, err)
	}
}
