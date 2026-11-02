package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

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

// dnsState is the last background records sync's failure ("" = it worked).
type dnsState struct {
	mu  sync.Mutex
	err string
	at  time.Time // when it failed
}

const dnsAlertKey = "alert.dns.records"

// dnsRetryEvery is how long a failed records sync waits before the tick runs it again.
const dnsRetryEvery = 5 * time.Minute

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
		o.dnsSync.err, o.dnsSync.at = err.Error(), time.Now()
		o.openProblem(context.Background(), dnsAlertKey, SevWarning, "DNS records are not in sync: "+err.Error())
		return
	}
	o.dnsSync.err = ""
	o.clearProblem(context.Background(), dnsAlertKey)
}

// retryDNS runs the records sync again once a failed one is dnsRetryEvery
// old; the 30s tick calls it, so an outage heals without a deploy.
func (o *Orchestrator) retryDNS() {
	o.dnsSync.mu.Lock()
	due := o.dnsSync.err != "" && time.Since(o.dnsSync.at) >= dnsRetryEvery
	o.dnsSync.mu.Unlock()
	if due {
		o.dnsKick.Kick()
	}
}

// DNSSyncError is the last background records sync's failure; "" while
// the last one worked.
func (o *Orchestrator) DNSSyncError() string {
	o.dnsSync.mu.Lock()
	defer o.dnsSync.mu.Unlock()
	return o.dnsSync.err
}

// WaitDNS returns once no background records sync is running: tests read
// the zones after it.
func (o *Orchestrator) WaitDNS() { o.dnsKick.Wait() }
