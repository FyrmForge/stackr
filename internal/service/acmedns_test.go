package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The controller's DNS socket, as the proxy's stackr module calls it: ACME
// challenge records of a managed domain land in its connector's account
// under the zone lock; anything else is refused before any provider call.
func TestACMEDNSSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "acme") // short: a unix socket path is capped
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	env := servicetest.New(t, func(c *service.Config) { c.ProxyAdmin = "unix://" + filepath.Join(dir, "admin.sock") })
	env.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "shop.io", OurDNS: true}}
	ctx := context.Background()
	c, err := env.Orch.AddConnector(ctx, service.ConnectorAdd{OrgID: nil, Kind: "namecheap", Values: account()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.ManageDNSDomain(ctx, nil, c.Connector.ID, "acme.dev", nil); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, worker.DNSSocket))
	}}}
	post := func(path, zone string, rs ...libdns.RR) (int, string) {
		b, _ := json.Marshal(worker.DNSRequest{Zone: zone, Records: rs})
		resp, err := hc.Post("http://stackrd"+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}
	txt := func(name, data string) libdns.RR { return libdns.RR{Name: name, Type: "TXT", Data: data} }

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, body := post(worker.DNSAppendPath, "acme.dev.", txt("_acme-challenge.app", "t"+string(rune('a'+i)))); code != http.StatusOK {
				t.Errorf("append: %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if code, body := post(worker.DNSDeletePath, "acme.dev", txt("_acme-challenge", "tok")); code != http.StatusOK || !strings.Contains(body, `"data":"tok"`) {
		t.Errorf("delete: %d %s", code, body)
	}
	appended, deleted, overlap := env.DNS.Log()
	if len(appended) != 6 || !strings.HasPrefix(appended[0], "acme.dev _acme-challenge.app t") {
		t.Errorf("appended = %v", appended)
	}
	if len(deleted) != 1 || deleted[0] != "acme.dev _acme-challenge tok" {
		t.Errorf("deleted = %v", deleted)
	}
	if overlap {
		t.Error("two calls were inside one zone at once: no zone lock")
	}

	for name, tc := range map[string]struct {
		zone string
		rr   libdns.RR
		code int
	}{
		"zone on the account, not managed": {"shop.io", txt("_acme-challenge", "x"), http.StatusForbidden},
		"zone nowhere":                     {"else.org", txt("_acme-challenge", "x"), http.StatusForbidden},
		"not a challenge name":             {"acme.dev", txt("www", "x"), http.StatusBadRequest},
		"absolute name":                    {"acme.dev", txt("_acme-challenge.evil.org.", "x"), http.StatusBadRequest},
		"not a TXT":                        {"acme.dev", libdns.RR{Name: "_acme-challenge", Type: "A", Data: "1.2.3.4"}, http.StatusBadRequest},
		"no value, a wildcard delete":      {"acme.dev", txt("_acme-challenge", ""), http.StatusBadRequest},
	} {
		if code, body := post(worker.DNSDeletePath, tc.zone, tc.rr); code != tc.code {
			t.Errorf("%s: %d %s, want %d", name, code, body, tc.code)
		}
	}
	if a, d, _ := env.DNS.Log(); len(a) != 6 || len(d) != 1 {
		t.Errorf("a refused call reached the provider: %v %v", a, d)
	}
	// the provider's own failure is a 502 the proxy retries, even when the
	// kind explains it as a refusal
	for _, fail := range []error{errs.Invalidf("key", "The provider refused the API key"), errs.Refusedf("The provider did not answer")} {
		env.DNS.Fail(fail)
		if code, body := post(worker.DNSAppendPath, "acme.dev", txt("_acme-challenge", "y")); code != http.StatusBadGateway {
			t.Errorf("provider failure %v: %d %s, want 502", fail, code, body)
		}
	}
}
