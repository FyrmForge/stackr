package service

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// fakeProbe answers certProbe from a host -> cert map; an absent host fails.
func fakeProbe(t *testing.T, certs map[string]*x509.Certificate) *[]string {
	t.Helper()
	var mu sync.Mutex
	var dialled []string
	old := certProbe
	t.Cleanup(func() { certProbe = old })
	certProbe = func(_ context.Context, addr, host string) (*x509.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		dialled = append(dialled, addr+" "+host)
		if c, ok := certs[host]; ok {
			return c, nil
		}
		return nil, errors.New("handshake failure")
	}
	return &dialled
}

func certFor(host string, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
}

// Hosts with a good certificate stay quiet; wildcard, HTTP-only and
// pass-through hosts are never dialled; a host without a certificate fires
// once, and cert.ok follows when it is right again. One box dials loopback.
func TestHostCertAlerts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	h := newHook(t)
	alertsTo(t, w.orch, h, "")
	tl := w.tile(t, "web", false)
	old := time.Now().Add(-48 * time.Hour)
	for i, d := range []store.Domain{
		{Host: "good.acme.dev", HTTPS: true},
		{Host: "bad.acme.dev", HTTPS: true},
		{Host: "*.acme.dev", HTTPS: true},
		{Host: "plain.acme.dev", HTTPS: false},
		{Host: "fresh.acme.dev", HTTPS: true},
	} {
		d.ID, d.TileID, d.Path, d.CreatedAt = "d"+string(rune('1'+i)), tl.ID, "/", old
		if d.Host == "fresh.acme.dev" {
			d.CreatedAt = time.Now()
		}
		must(t, w.st.Domains.Create(ctx, d))
	}
	_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "pve.other.dev", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)
	certs := map[string]*x509.Certificate{"good.acme.dev": certFor("good.acme.dev", time.Now().Add(60*24*time.Hour))}
	dialled := fakeProbe(t, certs)

	w.orch.hostCertAlerts(ctx)
	if got := h.events(t, w.orch); !slices.Equal(got, []string{AlertCertFailing + ":" + SevCritical}) {
		t.Errorf("events = %v", got)
	}
	slices.Sort(*dialled)
	if want := []string{"127.0.0.1 bad.acme.dev", "127.0.0.1 fresh.acme.dev", "127.0.0.1 good.acme.dev"}; !slices.Equal(*dialled, want) {
		t.Errorf("dialled = %v, want %v", *dialled, want)
	}

	w.orch.hostCertAlerts(ctx) // still failing: no repeat
	if got := h.events(t, w.orch); len(got) != 0 {
		t.Errorf("repeat = %v", got)
	}
	certs["bad.acme.dev"] = certFor("bad.acme.dev", time.Now().Add(60*24*time.Hour))
	w.orch.hostCertAlerts(ctx)
	if got := h.events(t, w.orch); !slices.Equal(got, []string{AlertCertOK + ":" + SevInfo}) {
		t.Errorf("recovery = %v", got)
	}
}

// A worker is dialled at its public address, not its mesh one; the controller
// by loopback; a worker with no public address is skipped.
func TestCertAddr(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cluster = &clusterPKI{}
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.WGAddress = "10.209.0.1"
	if got := n.orch.certAddr(ctx, w); got != "" {
		t.Errorf("no public address: %q", got)
	}
	w.PublicIP = "203.0.113.9"
	must(t, n.st.Nodes.Update(ctx, w))
	if got := n.orch.certAddr(ctx, w); got != "203.0.113.9" {
		t.Errorf("worker = %q", got)
	}
	if got := n.orch.certAddr(ctx, n.ctl); got != "127.0.0.1" {
		t.Errorf("controller = %q", got)
	}
}

func TestCertProblem(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	old := now.Add(-10 * day)
	for _, c := range []struct {
		name  string
		cert  *x509.Certificate
		err   error
		added time.Time
		bad   bool
	}{
		{"good", certFor("a.io", now.Add(60*day)), nil, old, false},
		{"expires soon", certFor("a.io", now.Add(10*day)), nil, old, true},
		{"expired", certFor("a.io", now.Add(-day)), nil, old, true},
		{"other name", certFor("b.io", now.Add(60*day)), nil, old, true},
		{"none, old host", nil, errors.New("x"), old, true},
		{"none, new host", nil, errors.New("x"), now.Add(-time.Hour), false},
		{"wildcard cert", certFor("*.io", now.Add(60*day)), nil, old, false},
	} {
		if got := certProblem(c.cert, c.err, "a.io", c.added, now) != ""; got != c.bad {
			t.Errorf("%s: bad = %v", c.name, got)
		}
	}
}
