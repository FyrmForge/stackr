package dnscap

import (
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Values are checked per type before anything is planned, and come back
// in the form the provider reads them.
func TestCustomValue(t *testing.T) {
	for _, c := range []struct {
		typ, value, want string
		ok               bool
	}{
		{"a", "198.51.100.7", "198.51.100.7", true},
		{"A", "2001:db8::1", "", false},
		{"A", "shop.example.com", "", false},
		{"AAAA", "2001:DB8::1", "2001:db8::1", true},
		{"AAAA", "198.51.100.7", "", false},
		{"AAAA", "::ffff:198.51.100.7", "", false},
		{"CNAME", "Shops.Example.NET.", "shops.example.net", true},
		{"CNAME", "localhost", "", false},
		{"CNAME", "has space.example.com", "", false},
		{"MX", "10 mx.example.net.", "10 mx.example.net", true},
		{"MX", "  5   MX.example.net", "5 mx.example.net", true},
		{"MX", "mx.example.net", "", false},
		{"MX", "70000 mx.example.net", "", false},
		{"TXT", `"v=spf1 include:_spf.example.net ~all"`, "v=spf1 include:_spf.example.net ~all", true},
		{"TXT", "", "", false},
		{"TXT", "two\nlines", "", false},
		{"SRV", "0 5 5060 sip.example.com", "", false},
	} {
		_, got, err := CustomValue(c.typ, c.value)
		if _, inv := errs.IsInvalid(err); c.ok != (err == nil) || !c.ok && !inv || got != c.want {
			t.Errorf("%s %q = %q %v, want %q ok=%v", c.typ, c.value, got, err, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		name, want string
		ok         bool
	}{
		{"", "", true}, {"@", "", true}, {"_DMARC", "_dmarc", true}, {"a.b", "a.b", true},
		{"bad name", "", false}, {"-x", "", false},
	} {
		got, err := CustomName(c.name)
		if c.ok != (err == nil) || got != c.want {
			t.Errorf("name %q = %q %v", c.name, got, err)
		}
	}
}

// A CNAME wants its name to itself, and nothing joins a CNAME; the same
// record again is no clash.
func TestClash(t *testing.T) {
	live := []libdns.Record{
		libdns.RR{Name: "shop", Type: "TXT", Data: "x"},
		libdns.RR{Name: "cdn", Type: "CNAME", Data: "edge.example.net."},
	}
	for _, c := range []struct {
		name, typ, value string
		clash            bool
	}{
		{"shop", "CNAME", "a.example.net", true},
		{"shop", "TXT", "y", false},
		{"cdn", "TXT", "y", true},
		{"cdn", "CNAME", "edge.example.net", false},
		{"cdn", "CNAME", "other.example.net", true},
		{"new", "CNAME", "a.example.net", false},
	} {
		if err := clash(live, "acme.dev", c.name, c.typ, c.value); (err != nil) != c.clash {
			t.Errorf("%s %s: %v", c.typ, c.name, err)
		}
	}
}

func TestSameData(t *testing.T) {
	for _, c := range []struct {
		typ, a, b string
		want      bool
	}{
		{"AAAA", "2001:db8::1", "2001:0db8:0:0::1", true},
		{"A", "1.2.3.4", "1.2.3.5", false},
		{"MX", "10 mx.example.net", "10 MX.example.net.", true},
		{"TXT", "v=spf1", "V=SPF1", false},
	} {
		if got := sameData(c.typ, c.a, c.b); got != c.want {
			t.Errorf("sameData(%s, %q, %q) = %v", c.typ, c.a, c.b, got)
		}
	}
}
