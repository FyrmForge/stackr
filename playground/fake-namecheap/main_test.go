package main

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func post(t *testing.T, s *server, cmd string, extra url.Values) string {
	t.Helper()
	q := url.Values{"ApiUser": {"u"}, "ApiKey": {"k"}, "UserName": {"u"}, "ClientIp": {"1.2.3.4"}, "Command": {cmd}}
	for k, v := range extra {
		q[k] = v
	}
	req := httptest.NewRequest("POST", "/xml.response", strings.NewReader(q.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handle(w, req)
	return w.Body.String()
}

func TestConnectorFlow(t *testing.T) {
	dir := t.TempDir()
	s, err := newServer(dir, "", []string{"stackr.test"})
	if err != nil {
		t.Fatal(err)
	}
	if out := post(t, s, "namecheap.domains.getList", url.Values{"PageSize": {"100"}, "Page": {"1"}}); !strings.Contains(out, `<Domain Name="stackr.test" IsOurDNS="true"/>`) || !strings.Contains(out, "<TotalItems>1</TotalItems>") {
		t.Fatal(out)
	}
	zone := url.Values{"SLD": {"stackr"}, "TLD": {"test"}}
	if out := post(t, s, "namecheap.domains.dns.getHosts", zone); !strings.Contains(out, `Name="@" Type="A" Address="203.0.113.1"`) {
		t.Fatal(out)
	}
	set := url.Values{"SLD": {"stackr"}, "TLD": {"test"}, "EmailType": {"MX"},
		"HostName1": {"@"}, "RecordType1": {"A"}, "Address1": {"203.0.113.1"}, "TTL1": {"1800"},
		"HostName2": {"mail"}, "RecordType2": {"MX"}, "Address2": {"mx.example.net."}, "MXPref2": {"20"},
		"HostName3": {"@"}, "RecordType3": {"CAA"}, "Flag3": {"0"}, "Tag3": {"issue"}, "Address3": {"ca.example"},
	}
	if out := post(t, s, "namecheap.domains.dns.setHosts", set); !strings.Contains(out, `Status="OK"`) {
		t.Fatal(out)
	}
	out := post(t, s, "namecheap.domains.dns.getHosts", zone)
	for _, want := range []string{`EmailType="MX"`, `Name="mail" Type="MX" Address="mx.example.net." MXPref="20"`, `Type="CAA" Address="0 issue ca.example"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "dnsmasq.conf")); err != nil || !strings.Contains(string(b), "mx-host=mail.stackr.test,mx.example.net,20") {
		t.Fatalf("dnsmasq.conf not rendered: %v", err)
	}
}

func TestErrors(t *testing.T) {
	s, _ := newServer(t.TempDir(), "", []string{"stackr.test"})
	out := post(t, s, "namecheap.domains.dns.getHosts", url.Values{"SLD": {"nope"}, "TLD": {"test"}})
	if !strings.Contains(out, `<Errors><Error Number="2019166">Domain not found</Error></Errors>`) || !strings.Contains(out, `Status="ERROR"`) {
		t.Fatal(out)
	}
}

func TestRender(t *testing.T) {
	z := map[string]*zone{"stackr.test": {Hosts: []host{
		{Name: "@", Type: "A", Address: "203.0.113.1"},
		{Name: "*.apps", Type: "A", Address: "203.0.113.2"},
		{Name: "www", Type: "CNAME", Address: "stackr.test."},
		{Name: "t", Type: "TXT", Address: `v=spf1 "x"`},
		{Name: "mail", Type: "MX", Address: "mx.example.net.", MXPref: "20"},
		{Name: "@", Type: "CAA", Address: "0 issue ca.example"},
	}}}
	got := render(z)
	for _, want := range []string{
		"host-record=stackr.test,203.0.113.1\n",
		"address=/apps.stackr.test/203.0.113.2\n",
		"cname=www.stackr.test,stackr.test\n",
		"txt-record=t.stackr.test,\"v=spf1 \\\"x\\\"\"\n",
		"mx-host=mail.stackr.test,mx.example.net,20\n",
		"dns-rr=stackr.test,257,0005697373756563612e6578616d706c65\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}
