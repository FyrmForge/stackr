package dnscap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// slowZone is a zone whose read and write each take a while: a writer that
// slips between another's read and write loses that one's change.
type slowZone struct {
	fake
	mu sync.Mutex
	rs []libdns.RR
}

func (z *slowZone) Get(context.Context, string) ([]libdns.Record, error) {
	z.mu.Lock()
	snap := append([]libdns.RR(nil), z.rs...)
	z.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	out := make([]libdns.Record, len(snap))
	for i, r := range snap {
		out[i] = r
	}
	return out, nil
}

// Set here replaces the whole zone, as some providers do.
func (z *slowZone) Set(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	time.Sleep(2 * time.Millisecond)
	z.mu.Lock()
	defer z.mu.Unlock()
	z.rs = z.rs[:0]
	for _, r := range rs {
		z.rs = append(z.rs, r.RR())
	}
	return rs, nil
}

// WithZone holds the zone's lock across a read and the write it decides:
// eight writers each adding one record all land.
func TestWithZoneHoldsLockAcrossReadAndWrite(t *testing.T) {
	z := &slowZone{}
	l := New(nil, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := l.WithZone(context.Background(), z, "example.com", func(p Provider) error {
				rs, err := p.Get(context.Background(), "example.com")
				if err != nil {
					return err
				}
				rs = append(rs, libdns.RR{Name: string(rune('a' + i)), Type: "A", Data: "198.51.100.1"})
				_, err = p.Set(context.Background(), "example.com", rs)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(z.rs) != 8 {
		t.Errorf("%d of 8 records landed: writers interleaved", len(z.rs))
	}
}

// checkWrite refuses a write that removes a record outside the scope or
// not stackr's, or adds one outside the scope; a write inside, on its own
// records, passes.
func TestCheckWrite(t *testing.T) {
	d := store.DNSDomain{Name: "acme.dev", Scope: "stackr.acme.dev"}
	whole := store.DNSDomain{Name: "acme.dev"}
	rec := func(name, ip string) libdns.Record { return libdns.RR{Name: name, Type: "A", Data: ip} }
	live := []libdns.Record{rec("www", "203.0.113.9"), rec("app.stackr", "198.51.100.1"), rec("hand.stackr", "203.0.113.5")}
	mine := func(r libdns.RR) bool { return r.Name == "app.stackr" || r.Name == "www" }
	cases := []struct {
		name     string
		d        store.DNSDomain
		del, put []libdns.Record
		ok       bool
	}{
		{"own record in scope", d, []libdns.Record{rec("app.stackr", "198.51.100.1")}, []libdns.Record{rec("app.stackr", "198.51.100.2")}, true},
		{"delete outside scope", d, []libdns.Record{rec("www", "203.0.113.9")}, nil, false},
		{"add outside scope", d, nil, []libdns.Record{rec("blog", "198.51.100.1")}, false},
		{"delete someone else's in scope", d, []libdns.Record{rec("hand.stackr", "203.0.113.5")}, nil, false},
		{"delete someone else's on a whole domain", whole, []libdns.Record{rec("hand.stackr", "203.0.113.5")}, nil, false},
		{"delete of a record not there", d, []libdns.Record{rec("gone", "203.0.113.1")}, nil, true},
	}
	for _, c := range cases {
		err := checkWrite(c.d, live, c.del, c.put, mine)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if err != nil && !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s: not a refusal: %v", c.name, err)
		}
	}
}
