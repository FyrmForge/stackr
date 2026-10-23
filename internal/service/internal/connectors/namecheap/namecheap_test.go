package namecheap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
)

// account is the test account, its calls sent to url.
func account(url string) *Account {
	return &Account{User: "u", Key: "k", ClientIP: "203.0.113.7", endpoint: url}
}

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
	n := account(srv.URL)
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
	_, err := account(srv.URL).Get(context.Background(), "fyrmforge.dev")
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "2019166") {
		t.Errorf("err = %v, want a refusal naming 2019166", err)
	}
}

// Namecheap's numbered errors come back naming the cause and the field.
func TestCheckErrors(t *testing.T) {
	answer := func(number, text string) string {
		return `<?xml version="1.0" encoding="utf-8"?><ApiResponse Status="ERROR" xmlns="http://api.namecheap.com/xml.response">` +
			`<Errors><Error Number="` + number + `">` + text + `</Error></Errors><CommandResponse/></ApiResponse>`
	}
	for _, tc := range []struct{ number, field, want string }{
		{"1011150", "client_ip", "203.0.113.7"},
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
		_, err := account(srv.URL).Zones(context.Background())
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
	if _, err := account(srv.URL).Zones(context.Background()); !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), "Domain is invalid") {
		t.Errorf("unknown error = %v", err)
	}
	// a missing or IPv6 client IP never reaches Namecheap
	for _, ip := range []string{"", "2001:db8::1", "nope"} {
		err := (&Account{}).Setup(map[string]string{"user": "u", "key": "k", "client_ip": ip})
		if inv, ok := errs.IsInvalid(err); !ok || inv.Field != "client_ip" {
			t.Errorf("ip %q: want an Invalid on client_ip", ip)
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
	zs, err := account(srv.URL).Zones(context.Background())
	want := []connectors.Zone{{Name: "a.com", OurDNS: true}, {Name: "b.io"}, {Name: "c.co.uk", OurDNS: true}}
	if err != nil || len(zs) != 3 || zs[0] != want[0] || zs[1] != want[1] || zs[2] != want[2] {
		t.Errorf("zones = %v %v, want %v", zs, err, want)
	}
}
