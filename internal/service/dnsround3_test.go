package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
)

var errProviderDown = errors.New("provider down")

// Two app records move (the node's address changed) and the provider fails
// after the first write. A provider that cannot change a zone in one write
// keeps the rows of what landed, so the next sync finishes the job instead
// of reading the new records as someone else's.
func TestDNSMoveFailingMidwayHeals(t *testing.T) {
	t.Parallel()
	env, _, _, tl := recordsRig(t)
	env.DNS.NoReplace()
	ctx := context.Background()
	for _, h := range []string{"shop.acme.dev", "blog.acme.dev"} {
		if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	env.Orch.WaitDNS()
	setIP(t, env, "198.51.100.2")
	env.DNS.FailAfter(1, errProviderDown)
	_ = env.Orch.SyncProxy(ctx)
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	if n := len(slices.DeleteFunc(slices.Clone(z), func(r libdns.RR) bool { return r.Data != "198.51.100.2" })); n != 1 {
		t.Fatalf("setup: exactly one record should have moved: %v", z)
	}
	env.DNS.Fail(nil)
	resync(t, env)
	for _, n := range []string{"shop", "blog"} {
		if z = env.DNS.Zone("acme.dev"); !has(z, n, "A", "198.51.100.2") || has(z, n, "A", "198.51.100.1") {
			t.Errorf("%s did not heal: %v", n, z)
		}
	}
	if p := env.Orch.DNSSyncError(); p != "" {
		t.Errorf("a record the sync wrote itself is reported taken: %s", p)
	}
	setIP(t, env, "198.51.100.3")
	resync(t, env)
	for _, n := range []string{"shop", "blog"} {
		if z = env.DNS.Zone("acme.dev"); !has(z, n, "A", "198.51.100.3") {
			t.Errorf("%s did not follow the node's address: %v", n, z)
		}
	}
}

// A provider with the one-write Replace changes every moved record of a
// zone in one write: a failure leaves the zone as it was.
func TestDNSMoveIsOneWriteWhenTheProviderCan(t *testing.T) {
	t.Parallel()
	env, _, _, tl := recordsRig(t)
	ctx := context.Background()
	for _, h := range []string{"shop.acme.dev", "blog.acme.dev"} {
		if _, err := env.Orch.AttachDomain(ctx, tl.ID, service.DomainSpec{Host: h}); err != nil {
			t.Fatal(err)
		}
	}
	env.Orch.WaitDNS()
	setIP(t, env, "198.51.100.2")
	env.DNS.FailAfter(0, errProviderDown)
	_ = env.Orch.SyncProxy(ctx)
	env.Orch.WaitDNS()
	z := env.DNS.Zone("acme.dev")
	if !has(z, "shop", "A", "198.51.100.1") || !has(z, "blog", "A", "198.51.100.1") || len(z) != 2 {
		t.Fatalf("a failed write left the zone half changed: %v", z)
	}
	// one write is allowed: with a provider that does a zone in one write
	// that is enough for both records
	env.DNS.FailAfter(1, errProviderDown)
	resync(t, env)
	if z = env.DNS.Zone("acme.dev"); !has(z, "shop", "A", "198.51.100.2") || !has(z, "blog", "A", "198.51.100.2") || len(z) != 2 {
		t.Errorf("both moves were not one write: %v", z)
	}
}

// panel_address ip:v4 -> ip:v6 while the provider refuses writes: the name
// keeps its record, with the one-write Replace and without it.
func TestDNSFamilySwitchNeverLeavesTheNameBare(t *testing.T) {
	t.Parallel()
	for _, plain := range []bool{false, true} {
		env, _, _ := scopedRig(t)
		if plain {
			env.DNS.NoReplace()
		}
		setSettings(t, env, map[string]string{"ipv6": "true"})
		setSettings(t, env, map[string]string{"panel_address": "ip:203.0.113.5"})
		if r := panelRecords(env); !has(r, "stackr", "A", "203.0.113.5") {
			t.Fatalf("plain=%v setup: %v", plain, r)
		}
		env.DNS.Fail(errProviderDown)
		_ = env.Orch.SetSettings(context.Background(), map[string]string{"panel_address": "ip:2001:db8::5"})
		env.Orch.WaitDNS()
		if r := panelRecords(env); len(r) == 0 {
			t.Errorf("plain=%v: the panel name has no record after a refused write: %v", plain, env.DNS.Zone("acme.dev"))
		}
		env.DNS.Fail(nil)
		resync(t, env)
		if r := panelRecords(env); !has(r, "stackr", "AAAA", "2001:db8::5") || has(r, "stackr", "A", "203.0.113.5") {
			t.Errorf("plain=%v: after the provider is back: %v", plain, r)
		}
	}
}

// Moving the panel name onto a name that has someone else's record: the old
// record stays until the new name has one of stackr's, then goes.
func TestPanelNameMoveHoldsOldRecordUntilTheNewOneExists(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	if r := panelRecords(env); !has(r, "stackr", "A", "198.51.100.1") {
		t.Fatalf("setup: %v", r)
	}
	env.DNS.Put("acme.dev", a("docs.stackr", "203.0.113.9"))
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"})
	z := env.DNS.Zone("acme.dev")
	if !has(z, "stackr", "A", "198.51.100.1") {
		t.Fatalf("the old panel name lost its record while the new one is blocked: %v", z)
	}
	if !has(z, "docs.stackr", "A", "203.0.113.9") {
		t.Errorf("someone else's record was touched: %v", z)
	}
	// the name is free again: the new record is written, then the old one goes
	env.DNS.Records["acme.dev"] = slices.DeleteFunc(env.DNS.Records["acme.dev"], func(r libdns.RR) bool { return r.Name == "docs.stackr" })
	resync(t, env)
	z = env.DNS.Zone("acme.dev")
	if !has(z, "docs.stackr", "A", "198.51.100.1") || has(z, "stackr", "A", "198.51.100.1") {
		t.Errorf("after the new name is free: %v", z)
	}
}
