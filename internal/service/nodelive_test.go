package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func TestGroupIfaces(t *testing.T) {
	f := worker.Facts{Interfaces: []worker.Iface{
		{Name: "lo", Prefixes: []string{"127.0.0.1/8", "::1/128"}},
		{Name: "eth0", Prefixes: []string{"203.0.113.7/24", "fe80::1/64"}, Public: true},
		{Name: "enp7s0", Prefixes: []string{"192.168.1.9/24", "100.64.1.2/32"}},
		{Name: "stackr0", Prefixes: []string{"10.213.0.2/16"}},
		{Name: "br-1a2b", Prefixes: []string{"10.208.3.1/24"}},
		{Name: "br-job", Prefixes: []string{"10.209.7.1/24"}},
		{Name: "br-other", Prefixes: []string{"172.18.0.1/16"}},
		{Name: "docker0", Prefixes: []string{"172.17.0.1/16"}},
		{Name: "veth9", Prefixes: []string{"10.208.3.9/24"}},
	}}
	got := groupIfaces(f, map[string]string{"10.208.3.0/24": "acme/shop/prod"})
	type row struct{ Group, Name, Label string }
	var rows []row
	for _, g := range got {
		rows = append(rows, row{g.Group, g.Name, g.Label})
	}
	want := []row{
		{IfaceHidden, "lo", "loopback"},
		{IfaceHidden, "lo", "loopback"},
		{IfacePublic, "eth0", ""},
		{IfaceHidden, "eth0", "link-local"},
		{IfaceLAN, "enp7s0", ""},
		{IfaceLAN, "enp7s0", ""}, // CGNAT is not public
		{IfaceCluster, "stackr0", "tunnel"},
		{IfaceCluster, "br-1a2b", "acme/shop/prod"},
		{IfaceCluster, "br-job", "stackr network"},
		{IfaceDocker, "br-other", ""},
		{IfaceDocker, "docker0", ""},
		{IfaceHidden, "veth9", "veth"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows = %+v\nwant   %+v", rows, want)
	}
}

func TestNodeLive(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Interfaces: []worker.Iface{{Name: "eth0", Prefixes: []string{"203.0.113.7/24"}}}})
	l, err := n.orch.NodeLive(ctx, "w1")
	must(t, err)
	if l.LastSeen.IsZero() || l.Facts == nil || len(l.Ifaces) != 1 || l.Ifaces[0].Group != IfacePublic {
		t.Fatalf("w1 = %+v", l)
	}
	c, err := n.orch.NodeLive(ctx, n.ctl.ID)
	must(t, err)
	if c.LastSeen.IsZero() || c.Facts == nil {
		t.Fatalf("controller = %+v", c)
	}
	if _, err := n.orch.NodeLive(ctx, "nope"); err == nil {
		t.Error("unknown node found")
	}
}

func TestCertNote(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cert := func(d time.Duration, gone bool) store.NodeCert {
		c := store.NodeCert{NotAfter: now.Add(d)}
		if gone {
			c.RevokedAt = &now
		}
		return c
	}
	for _, c := range []struct {
		name    string
		certs   []store.NodeCert
		expired bool
		days    int
	}{
		{"none", nil, false, 0},
		{"far", []store.NodeCert{cert(60*day, false)}, false, 0},
		{"exactly 14", []store.NodeCert{cert(14*day, false)}, false, 0},
		{"nine days", []store.NodeCert{cert(9*day, false)}, false, 9},
		{"part of a day counts", []store.NodeCert{cert(8*day+time.Hour, false)}, false, 9},
		{"expired", []store.NodeCert{cert(-day, false)}, true, 0},
		{"inside the clock slack", []store.NodeCert{cert(-time.Minute, false)}, false, 1},
		{"the newest decides", []store.NodeCert{cert(-day, false), cert(60*day, false)}, false, 0},
		{"revoked is not expired", []store.NodeCert{cert(-day, true)}, false, 0},
		{"revoked ignored", []store.NodeCert{cert(2*day, true), cert(-day, false)}, true, 0},
	} {
		if e, d := certNote(c.certs, now); e != c.expired || d != c.days {
			t.Errorf("%s: certNote = %v, %d; want %v, %d", c.name, e, d, c.expired, c.days)
		}
	}
}

// A worker on the panel's build is neither behind nor too old; an older one
// is behind; one nobody allows is too old; the controller is neither.
func TestNodeLiveVersion(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cfg.Version = "1.0.0"
	for _, c := range []struct {
		v           string
		behind, old bool
	}{{"1.0.0", false, false}, {"0.9.0", true, true}, {"", true, true}} {
		n.orch.mesh.Report(ctx, "w1", worker.Facts{Version: c.v})
		l, err := n.orch.NodeLive(ctx, "w1")
		must(t, err)
		if l.Behind != c.behind || l.TooOld != c.old {
			t.Errorf("version %q: behind %v, too old %v; want %v, %v", c.v, l.Behind, l.TooOld, c.behind, c.old)
		}
	}
	// the build the panel ran before its last upgrade is behind, not too old
	must(t, n.orch.store.Settings.Set(ctx, prevVersionKey, "0.9.0"))
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Version: "0.9.0"})
	if l, _ := n.orch.NodeLive(ctx, "w1"); !l.Behind || l.TooOld {
		t.Errorf("previous build: %+v", l)
	}
	c, _ := n.orch.NodeLive(ctx, n.ctl.ID)
	if c.Behind || c.TooOld {
		t.Errorf("controller: %+v", c)
	}
}
