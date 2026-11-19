package service

import (
	"context"
	"crypto/x509"
	"errors"
	"slices"
	"strings"
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

// A host removed while its certificate fails takes its alert and problem
// rows with it on the next complete read.
func TestHostCertSweepsRemovedHosts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	tl := w.tile(t, "web", false)
	must(t, w.st.Domains.Create(ctx, store.Domain{ID: "d1", TileID: tl.ID, Host: "bad.acme.dev", Path: "/", HTTPS: true,
		CreatedAt: time.Now().Add(-48 * time.Hour)}))
	fakeProbe(t, nil)
	w.orch.hostCertAlerts(ctx)
	rows, err := w.st.Settings.ListPrefix(ctx, "problem.certfail.")
	must(t, err)
	if len(rows) != 1 {
		t.Fatalf("problems = %v", rows)
	}
	must(t, w.st.Domains.Delete(ctx, "d1"))
	w.orch.hostCertAlerts(ctx)
	for _, p := range []string{"problem.certfail.", "alert.certfail."} {
		if rows, _ := w.st.Settings.ListPrefix(ctx, p); len(rows) != 0 {
			t.Errorf("%s rows kept: %v", p, rows)
		}
	}
}

// A worker is dialled at its public address, not its mesh one; the controller
// by loopback; a worker with no public address is skipped.
func TestCertAddr(t *testing.T) {
	// not parallel: swaps orch.cluster under the orchestrator's own background chores
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
	t.Parallel()
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

// A fresh install's panel name gets the grace: the boot check runs before the
// proxy has its certificate. Past the grace it fails, and the watch tick's
// recheck clears the problem as soon as the certificate is there.
func TestPanelCertGraceAndRecheck(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	must(t, w.orch.SetSetting(ctx, "panel_domain", "stkr.acme.dev"))
	certs := map[string]*x509.Certificate{}
	dialled := fakeProbe(t, certs)
	failing := func() int {
		rows, err := w.st.Settings.ListPrefix(ctx, "alert.certfail.")
		must(t, err)
		return len(rows)
	}

	w.orch.hostCertAlerts(ctx)
	if failing() != 0 {
		t.Fatal("a fresh install's panel name fired before its certificate could arrive")
	}
	w.orch.recheckFailingCerts(ctx)
	w.orch.trustAsync.Wait() // it runs off the tick
	if n := len(*dialled); n != 2 { // the name is still waiting, so the recheck looks again
		t.Fatalf("nothing failing and nothing waiting would not dial, this waits: %d dials", n)
	}

	ctl, err := w.orch.nodeLeaf().Controller(ctx)
	must(t, err)
	_, err = w.st.DB().ExecContext(ctx, "UPDATE nodes SET created_at = ? WHERE id = ?", time.Now().Add(-48*time.Hour), ctl.ID)
	must(t, err)
	w.orch.hostCertAlerts(ctx)
	if failing() != 1 {
		t.Fatal("an old install's panel name without a certificate stayed quiet")
	}
	certs["stkr.acme.dev"] = certFor("stkr.acme.dev", time.Now().Add(60*24*time.Hour))
	w.orch.recheckFailingCerts(ctx)
	w.orch.trustAsync.Wait()
	if failing() != 0 {
		t.Fatal("the recheck did not clear the problem once the certificate was there")
	}
}

// A host with no certificate reads failed with the reason, a young one
// waiting, and Check again now gives valid once the certificate is there.
func TestCertStateAndRecheck(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	tl := w.tile(t, "web", false)
	for i, d := range []store.Domain{{Host: "bad.acme.dev"}, {Host: "fresh.acme.dev"}} {
		d.ID, d.TileID, d.Path, d.HTTPS, d.CreatedAt = "s"+string(rune('1'+i)), tl.ID, "/", true, time.Now().Add(-48*time.Hour)
		if d.Host == "fresh.acme.dev" {
			d.CreatedAt = time.Now()
		}
		must(t, w.st.Domains.Create(ctx, d))
	}
	certs := map[string]*x509.Certificate{}
	fakeProbe(t, certs)
	if _, ok := w.orch.CertState("bad.acme.dev"); ok {
		t.Fatal("state before any check")
	}
	w.orch.hostCertAlerts(ctx)
	if s, _ := w.orch.CertState("bad.acme.dev"); s.Status != CertFailed || !strings.Contains(s.Reason, "no certificate") {
		t.Errorf("bad = %+v", s)
	}
	if s, _ := w.orch.CertState("fresh.acme.dev"); s.Status != CertWaiting {
		t.Errorf("fresh = %+v", s)
	}
	end := time.Now().Add(60 * 24 * time.Hour)
	certs["bad.acme.dev"] = certFor("bad.acme.dev", end)
	s, err := w.orch.RecheckCert(ctx, tl.ID, "bad.acme.dev")
	if err != nil || s.Status != CertValid || s.NotAfter.IsZero() {
		t.Errorf("recheck = %+v, %v", s, err)
	}
	if g, _ := w.orch.CertState("bad.acme.dev"); g.Status != CertValid {
		t.Errorf("stored = %+v", g)
	}
	if _, err := w.orch.RecheckCert(ctx, tl.ID, "other.dev"); err == nil {
		t.Error("a host of no domain of the tile was rechecked")
	}
}

// A host that left the inventory loses its state (a stale "waiting" would
// keep the recheck dialling every host), and a certificate inside the renewal
// window is valid with Soon, not failed.
func TestCertStateSweepAndSoon(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	tl := w.tile(t, "web", false)
	must(t, w.st.Domains.Create(ctx, store.Domain{ID: "s1", TileID: tl.ID, Host: "soon.acme.dev", Path: "/", HTTPS: true, CreatedAt: time.Now().Add(-48 * time.Hour)}))
	must(t, w.st.Domains.Create(ctx, store.Domain{ID: "s2", TileID: tl.ID, Host: "gone.acme.dev", Path: "/", HTTPS: true, CreatedAt: time.Now()}))
	certs := map[string]*x509.Certificate{"soon.acme.dev": certFor("soon.acme.dev", time.Now().Add(5*24*time.Hour))}
	fakeProbe(t, certs)
	w.orch.hostCertAlerts(ctx)
	if s, _ := w.orch.CertState("soon.acme.dev"); s.Status != CertValid || !s.Soon {
		t.Errorf("soon = %+v", s)
	}
	if s, _ := w.orch.CertState("gone.acme.dev"); s.Status != CertWaiting {
		t.Fatalf("gone = %+v", s)
	}
	must(t, w.st.Domains.Delete(ctx, "s2"))
	w.orch.hostCertAlerts(ctx)
	if _, ok := w.orch.CertState("gone.acme.dev"); ok {
		t.Error("the state of a detached host stayed")
	}
}

// The first tick after boot probes every host, so no row reads "not checked
// yet" until the daily pass.
func TestCertBootProbe(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	tl := w.tile(t, "web", false)
	must(t, w.st.Domains.Create(ctx, store.Domain{ID: "b1", TileID: tl.ID, Host: "ok.acme.dev", Path: "/", HTTPS: true, CreatedAt: time.Now().Add(-48 * time.Hour)}))
	fakeProbe(t, map[string]*x509.Certificate{"ok.acme.dev": certFor("ok.acme.dev", time.Now().Add(60*24*time.Hour))})
	w.orch.recheckFailingCerts(ctx)
	w.orch.trustAsync.Wait()
	if s, ok := w.orch.CertState("ok.acme.dev"); !ok || s.Status != CertValid {
		t.Errorf("after boot = %+v, %v", s, ok)
	}
}
