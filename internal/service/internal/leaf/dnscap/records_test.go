package dnscap

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/libdns/libdns"
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
