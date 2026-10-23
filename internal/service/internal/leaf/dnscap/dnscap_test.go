package dnscap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

var creds = Creds{User: "u", Key: "k", ClientIP: "203.0.113.7"}

func leafWith(t *testing.T, f *fake) *Leaf {
	st := storetest.Store(t)
	return New(st.DNSDomains, func(string, Creds) (Provider, error) { return f, nil })
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
			p, _ := l.Provider(Namecheap, creds)
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
			p, _ := l2.Provider(Namecheap, creds)
			_, _ = p.Set(context.Background(), z, nil)
		}()
	}
	wg.Wait()
	if m := f2.max.Load(); m > 2 {
		t.Errorf("max = %d", m)
	}
}

// Namecheap's numbered errors come back naming the cause and the field.
func TestCheckErrors(t *testing.T) {
	answer := func(number, text string) string {
		return `<?xml version="1.0" encoding="utf-8"?><ApiResponse Status="ERROR" xmlns="http://api.namecheap.com/xml.response">` +
			`<Errors><Error Number="` + number + `">` + text + `</Error></Errors><CommandResponse/></ApiResponse>`
	}
	for _, tc := range []struct{ number, field, want string }{
		{"1011150", "ip", "203.0.113.7"},
		{"1011102", "key", "API access is not turned on"},
		{"1017101", "user", "off or locked"},
		{"1019103", "user", "does not know"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" || r.FormValue("ApiKey") != "k" {
				t.Errorf("the key must go in the POST body only: query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(answer(tc.number, "nope")))
		}))
		l := New(nil, func(string, Creds) (Provider, error) { return newNamecheap(creds, srv.URL), nil })
		_, err := l.Check(context.Background(), Namecheap, creds)
		srv.Close()
		inv, ok := errs.IsInvalid(err)
		if !ok || inv.Field != tc.field || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an Invalid on %s naming %q", tc.number, err, tc.field, tc.want)
		}
	}
	// an unknown number is a refusal carrying Namecheap's words
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(answer("2030166", "Domain is invalid")))
	}))
	defer srv.Close()
	l := New(nil, func(string, Creds) (Provider, error) { return newNamecheap(creds, srv.URL), nil })
	if _, err := l.Check(context.Background(), Namecheap, creds); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Domain is invalid") {
		t.Errorf("unknown error = %v", err)
	}
	// a missing or IPv6 client IP never reaches Namecheap
	for _, ip := range []string{"", "2001:db8::1", "nope"} {
		c := creds
		c.ClientIP = ip
		if inv, ok := errs.IsInvalid(func() error { _, err := l.Check(context.Background(), Namecheap, c); return err }()); !ok || inv.Field != "ip" {
			t.Errorf("ip %q: want an Invalid on ip", ip)
		}
	}
}

// getList pages through the account and reads IsOurDNS.
func TestNamecheapZones(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("Command") != "namecheap.domains.getList" {
			t.Errorf("command %q", r.FormValue("Command"))
		}
		d := `<Domain Name="a.com" IsOurDNS="true"/><Domain Name="B.io" IsOurDNS="false"/>`
		if r.FormValue("Page") == "2" {
			d = `<Domain Name="c.co.uk" IsOurDNS="true"/>`
		}
		_, _ = w.Write([]byte(`<ApiResponse Status="OK"><Errors/><CommandResponse><DomainGetListResult>` + d +
			`</DomainGetListResult><Paging><TotalItems>3</TotalItems></Paging></CommandResponse></ApiResponse>`))
	}))
	defer srv.Close()
	zs, err := newNamecheap(creds, srv.URL).Zones(context.Background())
	want := []Zone{{"a.com", true}, {"b.io", false}, {"c.co.uk", true}}
	if err != nil || len(zs) != 3 || zs[0] != want[0] || zs[1] != want[1] || zs[2] != want[2] {
		t.Errorf("zones = %v %v, want %v", zs, err, want)
	}
}

// Take over needs the exact domain typed and never calls the provider
// otherwise; a domain already on Namecheap DNS is refused too.
func TestTakeOver(t *testing.T) {
	f := &fake{zones: []Zone{{"away.com", false}, {"home.com", true}}}
	l := leafWith(t, f)
	ctx := context.Background()
	for _, typed := range []string{"", "away", "AWAY.COM ", "away.com.x"} {
		if inv, ok := errs.IsInvalid(l.TakeOver(ctx, Namecheap, creds, "away.com", typed)); !ok || inv.Field != "confirm" {
			t.Errorf("typed %q: want an Invalid on confirm", typed)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("provider called %d times without a confirm", n)
	}
	if _, ok := errs.IsConflict(l.TakeOver(ctx, Namecheap, creds, "home.com", "home.com")); !ok {
		t.Error("taking over a domain already on Namecheap DNS: want a conflict")
	}
	if err := l.TakeOver(ctx, Namecheap, creds, "away.com", "away.com"); err != nil || len(f.tookOver) != 1 {
		t.Errorf("take over = %v, calls %v", err, f.tookOver)
	}
}

// Manage refuses a domain off the account or on DNS elsewhere; the list
// shows the three states.
func TestManageStates(t *testing.T) {
	f := &fake{zones: []Zone{{"away.com", false}, {"home.com", true}, {"free.com", true}}}
	l := leafWith(t, f)
	ctx := context.Background()
	if _, err := l.Manage(ctx, "c1", nil, Namecheap, creds, "other.com", nil); err == nil {
		t.Error("a domain off the account was managed")
	}
	if _, ok := errs.IsConflict(func() error { _, err := l.Manage(ctx, "c1", nil, Namecheap, creds, "away.com", nil); return err }()); !ok {
		t.Error("a DNS elsewhere domain: want a conflict")
	}
	ds, err := l.Domains(ctx, "c1", Namecheap, creds)
	if err != nil || len(ds) != 3 || ds[0].State != Elsewhere || ds[1].State != NotManaged {
		t.Errorf("domains = %+v %v", ds, err)
	}
}
