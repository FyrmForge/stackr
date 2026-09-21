package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// cloudflareBase serves Cloudflare's published edge ranges. A var so the test
// can point it at httptest.
var cloudflareBase = "https://www.cloudflare.com"

// cloudflareTimeout caps the whole fetch, both lists, so a Cloudflare outage
// stalls a config write by at most this.
var cloudflareTimeout = 3 * time.Second

// fetchCloudflare returns Cloudflare's v4 then v6 ranges. Any bad line fails
// the whole fetch, so a garbage page never replaces a good cache.
func fetchCloudflare(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, cloudflareTimeout)
	defer cancel()
	var out []string
	for _, path := range []string{"/ips-v4", "/ips-v6"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflareBase+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", path, resp.Status)
		}
		for _, line := range strings.Fields(string(body)) {
			if _, _, err := net.ParseCIDR(line); err != nil {
				return nil, fmt.Errorf("%s: not a CIDR: %s", path, line)
			}
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cloudflare returned no ranges")
	}
	return out, nil
}

// trustedIPs merges typed lines then the cache, dedupes, keeps order, and
// drops anything that is not a CIDR.
func trustedIPs(typed, cached string) []string {
	var out []string
	seen := map[string]bool{}
	for _, src := range []string{typed, cached} {
		sc := bufio.NewScanner(strings.NewReader(src))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if seen[line] {
				continue
			}
			if _, _, err := net.ParseCIDR(line); err != nil {
				continue
			}
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// RefreshCloudflare fetches Cloudflare's ranges and caches them. The cache is
// untouched on failure. The sort that keeps a reordered Cloudflare list from
// recreating Traefik lives with the write, in the settings owner.
func (p *Proxy) RefreshCloudflare(ctx context.Context) error {
	cidrs, err := fetchCloudflare(ctx)
	if err != nil {
		return err
	}
	return p.settings.SetCloudflareCIDRs(ctx, cidrs)
}

// trustedIPsLine renders forwardedHeaders for one entrypoint, or "" when
// nothing is trusted so the static config stays byte-identical.
func (p *Proxy) trustedIPsLine(ctx context.Context) string {
	typed, trustCF := p.settings.TrustedProxies(ctx)
	var cached string
	if trustCF {
		if err := p.RefreshCloudflare(ctx); err != nil {
			log.Printf("proxy: cloudflare ranges not refreshed, using cache: %v", err)
		}
		cached = p.settings.CloudflareCIDRs(ctx)
	}
	ips := trustedIPs(typed, cached)
	if len(ips) == 0 {
		return ""
	}
	quoted := make([]string, len(ips))
	for i, ip := range ips {
		quoted[i] = fmt.Sprintf("%q", ip)
	}
	return "    forwardedHeaders:\n      trustedIPs: [" + strings.Join(quoted, ", ") + "]\n"
}
