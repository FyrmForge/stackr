package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestDNSNote(t *testing.T) {
	here := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("203.0.113.7"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	lookup := func(ips ...string) func(string) ([]string, error) {
		return func(string) ([]string, error) { return ips, nil }
	}
	if n := dnsNote("p.example.com", lookup("203.0.113.7"), here); n != "" {
		t.Errorf("a host on this box: %q", n)
	}
	if n := dnsNote("p.example.com", lookup("198.51.100.1"), here); !strings.Contains(n, "198.51.100.1") {
		t.Errorf("a host elsewhere: %q", n)
	}
	fail := func(string) ([]string, error) { return nil, errors.New("no such host") }
	if n := dnsNote("p.example.com", fail, here); !strings.Contains(n, "does not resolve") {
		t.Errorf("an unresolved host: %q", n)
	}
}

// -h is not an error.
func TestHelpExitsClean(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, &out); err != nil {
		t.Fatalf("-h: %v", err)
	}
}
