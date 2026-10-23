package servicetest

import (
	"context"
	"sync"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// DNS is the fake DNS account every harness wires in: Zones is what each
// account lists; an account whose key is BadKey is refused like a wrong
// Namecheap key. TookOver records the take-overs asked for.
type DNS struct {
	mu       sync.Mutex
	Zones    []service.DNSZone
	TookOver []string
	Opened   []service.DNSCreds
}

// BadKey is the key the fake refuses.
const BadKey = "bad-key"

func (d *DNS) open(_ string, c service.DNSCreds) (service.DNSProvider, error) {
	d.mu.Lock()
	d.Opened = append(d.Opened, c)
	d.mu.Unlock()
	return fakeAccount{d, c}, nil
}

type fakeAccount struct {
	d *DNS
	c service.DNSCreds
}

func (a fakeAccount) Zones(context.Context) ([]service.DNSZone, error) {
	if a.c.Key == BadKey {
		return nil, errs.Invalidf("key", "Namecheap refused the API key")
	}
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	return append([]service.DNSZone(nil), a.d.Zones...), nil
}

func (fakeAccount) Get(context.Context, string) ([]libdns.Record, error) { return nil, nil }
func (fakeAccount) Append(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	return rs, nil
}
func (fakeAccount) Set(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	return rs, nil
}
func (fakeAccount) Delete(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	return rs, nil
}

func (a fakeAccount) TakeOver(_ context.Context, zone string) error {
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	a.d.TookOver = append(a.d.TookOver, zone)
	for i := range a.d.Zones {
		if a.d.Zones[i].Name == zone {
			a.d.Zones[i].OurDNS = true
		}
	}
	return nil
}
