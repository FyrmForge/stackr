package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

// coalesce runs fn off the caller, one run at a time: kicks that land
// while a run is going fold into one more run after it.
type coalesce struct {
	fn          func()
	mu          sync.Mutex
	busy, again bool
	wg          sync.WaitGroup // a run going; Wait is for tests
}

func (k *coalesce) Kick() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.busy {
		k.again = true
		return
	}
	k.busy = true
	k.wg.Add(1)
	go k.loop()
}

func (k *coalesce) loop() {
	defer k.wg.Done()
	for {
		k.fn()
		k.mu.Lock()
		if !k.again {
			k.busy = false
			k.mu.Unlock()
			return
		}
		k.again = false
		k.mu.Unlock()
	}
}

// Wait returns once no run is going.
func (k *coalesce) Wait() { k.wg.Wait() }

// dnsState is the last background records sync's failure (nil = it worked).
type dnsState struct {
	mu  sync.Mutex
	err error
}

// syncDNS is the proxy push's follow-up, kicked after a run whose pushes
// landed: the records sync runs on its own, under its own time limit, so a
// slow provider never holds a proxy push or a deploy. Its failure is
// logged and kept for the DNS connectors section (DNSSyncError); the next
// push tries again.
func (o *Orchestrator) syncDNS() {
	ctx, cancel := context.WithTimeout(context.Background(), domain.Timeout)
	defer cancel()
	err := o.reconcileDNS(ctx, nil)
	o.dnsSync.mu.Lock()
	defer o.dnsSync.mu.Unlock()
	if err != nil {
		slog.Warn("dns: records", "err", err)
	}
	o.dnsSync.err = err
}

// DNSSyncError is the last background records sync's failure; "" while
// the last one worked.
func (o *Orchestrator) DNSSyncError() string {
	o.dnsSync.mu.Lock()
	defer o.dnsSync.mu.Unlock()
	if o.dnsSync.err == nil {
		return ""
	}
	return o.dnsSync.err.Error()
}

// DNSSyncErrorFor is DNSSyncError as orgID's connectors list shows it
// (nil = the server's, all of it): an org sees only the parts about the
// domains its own connectors manage, since the rest can name other orgs'
// hosts.
func (o *Orchestrator) DNSSyncErrorFor(ctx context.Context, orgID *string) string {
	if orgID == nil {
		return o.DNSSyncError()
	}
	o.dnsSync.mu.Lock()
	err := o.dnsSync.err
	o.dnsSync.mu.Unlock()
	if err == nil {
		return ""
	}
	zones, zerr := o.dns.All(ctx)
	if zerr != nil {
		return ""
	}
	var mine []error
	for _, z := range zones {
		if z.OrgID != nil && *z.OrgID == *orgID {
			mine = append(mine, zoneErrs(err, z.Name)...)
		}
	}
	if len(mine) == 0 {
		return ""
	}
	return errors.Join(mine...).Error()
}

// zoneErrs is the parts of a sync's error about zone.
func zoneErrs(err error, zone string) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, x := range j.Unwrap() {
			out = append(out, zoneErrs(x, zone)...)
		}
		return out
	}
	if ze := (*dnscap.ZoneError)(nil); errors.As(err, &ze) && ze.Zone == zone {
		return []error{err}
	}
	return nil
}

// WaitDNS returns once no background records sync is running: tests read
// the zones after it.
func (o *Orchestrator) WaitDNS() { o.dnsKick.Wait() }
