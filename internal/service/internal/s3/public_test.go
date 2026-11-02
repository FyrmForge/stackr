package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for a, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "192.168.1.106": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::ffff:127.0.0.1": false, "fe80::1": false,
	} {
		if got := publicIP(netip.MustParseAddr(a)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", a, got, want)
		}
	}
}

// A PublicOnly bucket cannot reach a loopback server, an open one can.
func TestPublicOnlyRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	b := Bucket{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"}
	if err := Probe(context.Background(), b); err != nil {
		t.Fatalf("open bucket: %v", err)
	}
	b.PublicOnly = true
	if err := Probe(context.Background(), b); err == nil {
		t.Fatal("public-only bucket reached loopback")
	}
}
