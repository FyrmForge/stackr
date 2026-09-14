package proxy

// The panel hands routes to Traefik by writing files and, until this, never
// learned the outcome. The file watch is the only link and it can die
// silently: deleting the bind-mounted data dir under a running Traefik kills
// the inotify watch for good, and every route written afterwards is ignored
// (seen live 2026-09-01, a healthy tile serving 502 with clean logs).
//
// So ask Traefik. It answers 404 for a hostname it has no router for and
// something else for one it knows, which is the whole question. 404 after a
// grace period means the config never landed; restart the container in place
// and ask again.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// probeAttempts x probeInterval is how long a freshly written route gets to
// show up before we call the watch dead. Traefik reloads a file provider in
// well under a second; ten is slack for a loaded box.
const probeAttempts = 10

// restartMu serializes recovery so one apply's ten route writes trigger one
// restart, not ten. Verifiers that arrive during a restart wait it out and
// re-probe, which is why the second probe below is not redundant.
var restartMu sync.Mutex

// probeHost turns a router host into something we can put in a Host header.
// A wildcard router is a HostRegexp for one label, so any label matches.
func probeHost(host string) string {
	if base, ok := strings.CutPrefix(host, "*."); ok {
		return "stackr-probe." + base
	}
	return host
}

// probe reports whether Traefik currently has a router for host.
//
// A transport error counts as loaded, not missing: the panel may not be in
// docker at all (`hamr dev`), or Traefik may be mid-recreate. Neither is
// "route not loaded", and restarting on either would be worse than the bug.
func (p *Proxy) probe(host string) bool {
	req, err := http.NewRequest(http.MethodGet, p.probeURL, nil)
	if err != nil {
		return true
	}
	req.Host = probeHost(host)
	resp, err := probeClient.Do(req)
	if err != nil {
		log.Printf("proxy: cannot reach traefik to check %s: %v", host, err)
		return true
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}

// probeClient never follows redirects: a route that answers 301 is a loaded
// route, and following it would leave the docker network.
var probeClient = &http.Client{
	Timeout:       3 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (p *Proxy) waitRoute(host string) bool {
	for range probeAttempts {
		if p.probe(host) {
			return true
		}
		time.Sleep(p.probeInterval)
	}
	return false
}

// verify blocks until Traefik serves a router for host, restarting it once if
// it does not. Callers on a request or deploy path run it in a goroutine.
func (p *Proxy) verify(host string) {
	if p.probeURL == "" || host == "" {
		return // not configured (tests construct Proxy directly)
	}
	if p.waitRoute(host) {
		return
	}
	restartMu.Lock()
	defer restartMu.Unlock()
	if p.probe(host) {
		return // somebody else's restart already fixed it
	}
	log.Printf("proxy: traefik did not load route for %s, restarting", host)
	if err := p.restart(); err != nil {
		log.Printf("proxy: restart traefik: %v", err)
		return
	}
	if p.waitRoute(host) {
		log.Printf("proxy: route for %s live after restart", host)
		return
	}
	log.Printf("proxy: route for %s still missing after restarting traefik", host)
}

// restartTraefik rolls the Traefik service. The task comes back on a fresh
// container with fresh addresses, so the per-environment PROXY_IP values are
// re-recorded once it has settled, leaving them stale is a silent break for
// any config that reads them.
func (p *Proxy) restartTraefik() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// Baselined before the restart, or a stale "previous update completed"
	// with the old task still up reads as settled and the addresses below come
	// off the task that is about to die.
	since := time.Now()
	if err := p.rt.RestartService(ctx, runtime.TraefikService, 1); err != nil {
		return err
	}
	if err := p.rt.WaitRolled(ctx, runtime.TraefikService, since, 90*time.Second); err != nil {
		return err
	}
	nets, err := p.rt.ListNetworks(ctx, netpool.EnvPrefix)
	if err != nil {
		return err
	}
	p.recordAddrs(ctx, nets)
	return nil
}
