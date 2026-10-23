package dnscap

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// fake is an account whose writes are a slow read-modify-write: two
// interleaved writers would both see inside > 1.
type fake struct {
	zones       []Zone
	inside, max atomic.Int32
	calls       atomic.Int32
	tookOver    []string
	mu          sync.Mutex
}

func (f *fake) Zones(context.Context) ([]Zone, error) { f.calls.Add(1); return f.zones, nil }

func (f *fake) write() {
	f.calls.Add(1)
	n := f.inside.Add(1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	f.inside.Add(-1)
}

func (f *fake) Get(context.Context, string) ([]libdns.Record, error) { f.write(); return nil, nil }
func (f *fake) Append(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	f.write()
	return rs, nil
}
func (f *fake) Set(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	f.write()
	return rs, nil
}
func (f *fake) Delete(_ context.Context, _ string, rs []libdns.Record) ([]libdns.Record, error) {
	f.write()
	return rs, nil
}
func (f *fake) TakeOver(_ context.Context, z string) error {
	f.write()
	f.mu.Lock()
	f.tookOver = append(f.tookOver, z)
	f.mu.Unlock()
	return nil
}

func leafWith(t *testing.T, f *fake) *Leaf {
	st := storetest.Store(t)
	return New(st.DNSDomains, st.DNSRecords)
}

// Two writers on one zone never interleave, through two Provider values
// (each call builds its own); two zones run side by side.
func TestZoneLock(t *testing.T) {
	f := &fake{}
	l := leafWith(t, f)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := l.Locked(f)
			zone := "Example.com."
			if i%2 == 0 {
				zone = "example.com"
			}
			_, _ = p.Set(context.Background(), zone, nil)
			_, _ = p.Append(context.Background(), zone, nil)
		}()
	}
	wg.Wait()
	if m := f.max.Load(); m != 1 {
		t.Errorf("max writers inside one zone = %d, want 1", m)
	}

	f2 := &fake{}
	l2 := leafWith(t, f2)
	for _, z := range []string{"a.com", "b.com"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := l2.Locked(f2)
			_, _ = p.Set(context.Background(), z, nil)
		}()
	}
	wg.Wait()
	if m := f2.max.Load(); m > 2 {
		t.Errorf("max = %d", m)
	}
}

// Take over needs the exact domain typed and never calls the provider
// otherwise; a domain already on Namecheap DNS is refused too.
func TestTakeOver(t *testing.T) {
	f := &fake{zones: []Zone{{Name: "away.com"}, {Name: "home.com", OurDNS: true}}}
	l := leafWith(t, f)
	ctx := context.Background()
	for _, typed := range []string{"", "away", "AWAY.COM ", "away.com.x"} {
		if inv, ok := errs.IsInvalid(l.TakeOver(ctx, f, "Namecheap", "away.com", typed)); !ok || inv.Field != "confirm" {
			t.Errorf("typed %q: want an Invalid on confirm", typed)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("provider called %d times without a confirm", n)
	}
	if _, ok := errs.IsConflict(l.TakeOver(ctx, f, "Namecheap", "home.com", "home.com")); !ok {
		t.Error("taking over a domain already on Namecheap DNS: want a conflict")
	}
	if err := l.TakeOver(ctx, f, "Namecheap", "away.com", "away.com"); err != nil || len(f.tookOver) != 1 {
		t.Errorf("take over = %v, calls %v", err, f.tookOver)
	}
}

// Manage refuses a domain off the account or on DNS elsewhere; the list
// shows the three states.
func TestManageStates(t *testing.T) {
	f := &fake{zones: []Zone{{Name: "away.com"}, {Name: "home.com", OurDNS: true}, {Name: "free.com", OurDNS: true}}}
	l := leafWith(t, f)
	ctx := context.Background()
	if _, err := l.Manage(ctx, "c1", nil, f, "other.com", nil); err == nil {
		t.Error("a domain off the account was managed")
	}
	if _, ok := errs.IsConflict(func() error { _, err := l.Manage(ctx, "c1", nil, f, "away.com", nil); return err }()); !ok {
		t.Error("a DNS elsewhere domain: want a conflict")
	}
	ds, err := l.Domains(ctx, "c1", f)
	if err != nil || len(ds) != 3 || ds[0].State != Elsewhere || ds[1].State != NotManaged {
		t.Errorf("domains = %+v %v", ds, err)
	}
}
