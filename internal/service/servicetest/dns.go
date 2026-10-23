package servicetest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// DNS is the fake DNS account every harness wires in: Zones is what each
// account lists; an account whose key is BadKey is refused like a wrong
// Namecheap key. TookOver records the take-overs asked for. Appended and
// Deleted are the record calls ("zone name data"); Overlap is set when two
// calls were ever inside one zone at once.
type DNS struct {
	mu       sync.Mutex
	Zones    []service.DNSZone
	TookOver []string
	Opened   []service.DNSCreds
	Records  map[string][]libdns.RR // zone -> records
	Calls    int                    // record calls made
	Appended []string
	Deleted  []string
	Overlap  bool
	inZone   map[string]int
}

// record logs rs under zone into to, holding the zone a moment so two
// unlocked writers would overlap.
func (d *DNS) record(zone string, rs []libdns.Record, to *[]string) {
	d.mu.Lock()
	if d.inZone == nil {
		d.inZone = map[string]int{}
	}
	d.inZone[zone]++
	if d.inZone[zone] > 1 {
		d.Overlap = true
	}
	d.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inZone[zone]--
	for _, r := range rs {
		rr := r.RR()
		*to = append(*to, zone+" "+rr.Name+" "+rr.Data)
	}
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

// Get is the zone's records as set by the test (Records) and the writes.
func (a fakeAccount) Get(_ context.Context, zone string) ([]libdns.Record, error) {
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	a.d.Calls++
	var out []libdns.Record
	for _, r := range a.d.Records[zone] {
		out = append(out, r)
	}
	return out, nil
}

func (a fakeAccount) Append(_ context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	a.d.record(zone, rs, &a.d.Appended)
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	a.d.Calls++
	if a.d.Records == nil {
		a.d.Records = map[string][]libdns.RR{}
	}
	for _, r := range rs {
		if !slices.Contains(a.d.Records[zone], r.RR()) {
			a.d.Records[zone] = append(a.d.Records[zone], r.RR())
		}
	}
	return rs, nil
}

func (a fakeAccount) Set(_ context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	a.d.Calls++
	if a.d.Records == nil {
		a.d.Records = map[string][]libdns.RR{}
	}
	for _, r := range rs {
		rr := r.RR()
		a.d.Records[zone] = slices.DeleteFunc(a.d.Records[zone], func(x libdns.RR) bool { return x.Name == rr.Name && x.Type == rr.Type })
	}
	for _, r := range rs {
		a.d.Records[zone] = append(a.d.Records[zone], r.RR())
	}
	return rs, nil
}

// Delete removes records by name, type and value.
func (a fakeAccount) Delete(_ context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	a.d.record(zone, rs, &a.d.Deleted)
	a.d.mu.Lock()
	defer a.d.mu.Unlock()
	a.d.Calls++
	a.d.Records[zone] = slices.DeleteFunc(a.d.Records[zone], func(x libdns.RR) bool {
		return slices.ContainsFunc(rs, func(r libdns.Record) bool {
			rr := r.RR()
			return rr.Name == x.Name && rr.Type == x.Type && rr.Data == x.Data
		})
	})
	return rs, nil
}

// Zone is zone's records now, for a test to read.
func (d *DNS) Zone(zone string) []libdns.RR {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.Records[zone])
}

// Put sets zone's records, someone else's: what was there before stackr.
func (d *DNS) Put(zone string, rs ...libdns.RR) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Records == nil {
		d.Records = map[string][]libdns.RR{}
	}
	d.Records[zone] = append(d.Records[zone], rs...)
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

// Log is Appended, Deleted and Overlap read under the lock.
func (d *DNS) Log() (appended, deleted []string, overlap bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.Appended...), append([]string(nil), d.Deleted...), d.Overlap
}
