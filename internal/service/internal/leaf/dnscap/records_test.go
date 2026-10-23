package dnscap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// One split for every Namecheap call: the TLD is the public suffix, the
// SLD the label before it, a name under a domain refused.
func TestSplit(t *testing.T) {
	for _, tc := range []struct{ zone, sld, tld string }{
		{"fyrmforge.dev", "fyrmforge", "dev"},
		{"Shop.CO.UK.", "shop", "co.uk"},
		{"x.eco", "x", "eco"},
		{"a.com.au", "a", "com.au"},
	} {
		sld, tld, err := Split(tc.zone)
		if err != nil || sld != tc.sld || tld != tc.tld {
			t.Errorf("%s = %q %q %v, want %q %q", tc.zone, sld, tld, err, tc.sld, tc.tld)
		}
	}
	for _, bad := range []string{"www.fyrmforge.dev", "co.uk", "dev", ""} {
		if _, _, err := Split(bad); err == nil {
			t.Errorf("%q split", bad)
		}
	}
}

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

// Set here replaces the whole zone, as Namecheap's setHosts does.
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
	l := New(nil, nil, func(string, Creds) (Provider, error) { return z, nil })
	a := Account{Kind: Namecheap, Creds: creds}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := l.WithZone(context.Background(), a, "example.com", func(p Provider) error {
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

// hostsServer answers getHosts with the zone below and records the last
// setHosts body.
func hostsServer(t *testing.T, set *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("SLD") != "shop" || r.FormValue("TLD") != "co.uk" {
			t.Errorf("split %q %q", r.FormValue("SLD"), r.FormValue("TLD"))
		}
		switch r.FormValue("Command") {
		case "namecheap.domains.dns.getHosts":
			_, _ = w.Write([]byte(`<ApiResponse Status="OK"><Errors/><CommandResponse>` +
				`<DomainDNSGetHostsResult Domain="shop.co.uk" EmailType="FWD" IsUsingOurDNS="true">` +
				`<host HostId="1" Name="@" Type="A" Address="109.148.70.232" MXPref="10" TTL="1799" IsActive="true"/>` +
				`<host HostId="2" Name="mail" Type="MX" Address="mx.example.net." MXPref="20" TTL="1800" IsActive="true"/>` +
				`</DomainDNSGetHostsResult></CommandResponse></ApiResponse>`))
		case "namecheap.domains.dns.setHosts":
			_ = r.ParseForm()
			var kv []string
			for k, v := range r.PostForm {
				if k != "ApiKey" && k != "ApiUser" && k != "UserName" && k != "ClientIp" && k != "Command" {
					kv = append(kv, k+"="+v[0])
				}
			}
			*set = kv
			_, _ = w.Write([]byte(`<ApiResponse Status="OK"><Errors/><CommandResponse/></ApiResponse>`))
		}
	}))
}

// An append keeps every record it did not touch as read (TTL, MX
// preference) and the zone's mail setting; a delete takes only the exact
// record.
func TestNamecheapHostsRoundTrip(t *testing.T) {
	var set []string
	srv := hostsServer(t, &set)
	defer srv.Close()
	n := newNamecheap(creds, srv.URL)
	ctx := context.Background()
	if _, err := n.Append(ctx, "shop.co.uk", []libdns.Record{libdns.RR{Name: "api", Type: "A", Data: "198.51.100.1", TTL: 300 * time.Second}}); err != nil {
		t.Fatal(err)
	}
	got := "|" + strings.Join(set, "|") + "|"
	for _, want := range []string{
		"EmailType=FWD", "HostName1=@", "Address1=109.148.70.232", "TTL1=1799",
		"HostName2=mail", "RecordType2=MX", "MXPref2=20", "TTL2=1800",
		"HostName3=api", "Address3=198.51.100.1", "TTL3=300",
	} {
		if !strings.Contains(got, "|"+want+"|") {
			t.Errorf("setHosts lacks %s: %s", want, got)
		}
	}
	set = nil
	if _, err := n.Delete(ctx, "shop.co.uk", []libdns.Record{libdns.RR{Name: "@", Type: "A", Data: "203.0.113.1"}}); err != nil || set != nil {
		t.Errorf("deleting a record that is not there wrote %v %v", set, err)
	}
	if _, err := n.Delete(ctx, "shop.co.uk", []libdns.Record{libdns.RR{Name: "@", Type: "A", Data: "109.148.70.232"}}); err != nil {
		t.Fatal(err)
	}
	got = "|" + strings.Join(set, "|") + "|"
	if strings.Contains(got, "109.148.70.232") || !strings.Contains(got, "|HostName1=mail|") || !strings.Contains(got, "|EmailType=FWD|") {
		t.Errorf("after delete: %s", got)
	}
}

// A record call's refusal keeps Namecheap's number.
func TestRecordCallErrorNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<ApiResponse Status="ERROR"><Errors><Error Number="2019166">Domain not found</Error></Errors></ApiResponse>`))
	}))
	defer srv.Close()
	l := New(nil, nil, func(string, Creds) (Provider, error) { return newNamecheap(creds, srv.URL), nil })
	err := l.WithZone(context.Background(), Account{Kind: Namecheap, Creds: creds}, "fyrmforge.dev", func(p Provider) error {
		_, err := p.Get(context.Background(), "fyrmforge.dev")
		return err
	})
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "2019166") {
		t.Errorf("err = %v, want a refusal naming 2019166", err)
	}
}
