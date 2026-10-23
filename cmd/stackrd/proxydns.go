package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/worker"
)

func init() { caddy.RegisterModule(stackrDNS{}) }

// stackrDNS is `dns.providers.stackr`: the proxy's DNS-01 provider. It holds
// no key; every record change goes to stackrd over the DNS socket in the
// admin dir (the controller answers it, a worker passes it on), so the
// config Caddy autosaves names no secret.
type stackrDNS struct{}

func (stackrDNS) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "dns.providers.stackr", New: func() caddy.Module { return stackrDNS{} }}
}

var dnsClient = &http.Client{
	Timeout: 2 * time.Minute, // a Namecheap zone rewrite behind the zone lock
	Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(installspec.ProxyAdminDir, worker.DNSSocket))
	}},
}

func (stackrDNS) AppendRecords(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return dnsCall(ctx, dnsClient, worker.DNSAppendPath, zone, rs)
}

func (stackrDNS) DeleteRecords(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	return dnsCall(ctx, dnsClient, worker.DNSDeletePath, zone, rs)
}

func dnsCall(ctx context.Context, c *http.Client, path, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	in := worker.DNSRequest{Zone: zone, Records: make([]libdns.RR, len(rs))}
	for i, r := range rs {
		in.Records[i] = r.RR()
	}
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://stackrd"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stackr dns: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stackr dns: %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out worker.DNSRequest
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("stackr dns: unreadable answer: %w", err)
	}
	recs := make([]libdns.Record, len(out.Records))
	for i, r := range out.Records {
		recs[i] = r
	}
	return recs, nil
}

var (
	_ libdns.RecordAppender = stackrDNS{}
	_ libdns.RecordDeleter  = stackrDNS{}
)
