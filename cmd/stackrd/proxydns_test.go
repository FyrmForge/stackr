package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/worker"
)

// The module is registered under the id the builder names, and a record
// change goes out as a DNSRequest and comes back as libdns records.
func TestStackrDNSModule(t *testing.T) {
	if _, err := caddy.GetModule("dns.providers.stackr"); err != nil {
		t.Fatal(err)
	}
	var seen worker.DNSRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		if r.URL.Path != worker.DNSAppendPath {
			http.Error(w, "acme.dev is not a managed domain", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(seen)
	}))
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}}}
	in := []libdns.Record{libdns.TXT{Name: "_acme-challenge.app", TTL: time.Minute, Text: "tok"}}
	got, err := dnsCall(context.Background(), c, worker.DNSAppendPath, "acme.dev.", in)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Zone != "acme.dev." || len(seen.Records) != 1 || seen.Records[0] != (libdns.RR{Name: "_acme-challenge.app", TTL: time.Minute, Type: "TXT", Data: "tok"}) {
		t.Errorf("sent %+v", seen)
	}
	if len(got) != 1 || got[0].RR() != seen.Records[0] {
		t.Errorf("answer %+v", got)
	}
	if _, err := dnsCall(context.Background(), c, worker.DNSDeletePath, "acme.dev.", in); err == nil || !strings.Contains(err.Error(), "not a managed domain") {
		t.Errorf("a refusal: %v", err)
	}
}
