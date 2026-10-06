package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

func prefixes(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestFirstFreeSubnet(t *testing.T) {
	for name, tc := range map[string]struct {
		used []string
		want string
	}{
		"empty":         {nil, "10.213.0.0/24"},
		"skips used":    {[]string{"10.213.0.0/24", "10.213.1.0/24"}, "10.213.2.0/24"},
		"wider overlap": {[]string{"10.213.0.0/23"}, "10.213.2.0/24"},
		"host address":  {[]string{"10.213.0.7/32"}, "10.213.1.0/24"},
		"gap is reused": {[]string{"10.213.0.0/24", "10.213.2.0/24"}, "10.213.1.0/24"},
		"foreign nets":  {[]string{"172.17.0.0/16", "10.0.0.0/24"}, "10.213.0.0/24"},
		"ipv6 ignored":  {[]string{"fd00::/64"}, "10.213.0.0/24"},
		"last block":    {[]string{"10.213.0.0/17", "10.213.128.0/18", "10.213.192.0/19", "10.213.224.0/20", "10.213.240.0/21", "10.213.248.0/22", "10.213.252.0/23", "10.213.254.0/24"}, "10.213.255.0/24"},
	} {
		got, ok := firstFreeSubnet(prefixes(tc.used...))
		if !ok || got.String() != tc.want {
			t.Errorf("%s: got %v %v, want %s", name, got, ok, tc.want)
		}
	}
}

func TestUsedSubnets(t *testing.T) {
	nets := []network.Summary{
		{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "172.17.0.0/16", Gateway: "172.17.0.1"}}}},
		{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.213.4.0/24"}, {Subnet: "fd00:1::/64"}}}},
		{IPAM: network.IPAM{Config: []network.IPAMConfig{{}}}}, // no subnet
		{}, // host / none: no IPAM config
	}
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("10.213.9.5"), Mask: net.CIDRMask(24, 32)},  // narrow: whole subnet
		&net.IPNet{IP: net.ParseIP("10.0.0.5"), Mask: net.CIDRMask(8, 32)},     // wide LAN: address only
		&net.IPNet{IP: net.ParseIP("10.213.12.3"), Mask: net.CIDRMask(32, 32)}, // /32
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},    // link-local v6
		&net.IPAddr{IP: net.ParseIP("10.213.20.1")},                            // not an IPNet
	}
	used := usedSubnets(nets, addrs)
	for _, blocked := range []string{"10.213.4.0/24", "10.213.9.0/24", "10.213.12.0/24", "172.17.5.0/24"} {
		hit := false
		for _, u := range used {
			hit = hit || u.Overlaps(netip.MustParsePrefix(blocked))
		}
		if !hit {
			t.Errorf("%s not blocked", blocked)
		}
	}
	for _, free := range []string{"10.213.0.0/24", "10.213.20.0/24", "10.0.1.0/24"} {
		for _, u := range used {
			if u.Overlaps(netip.MustParsePrefix(free)) {
				t.Errorf("%s blocked by %s", free, u)
			}
		}
	}

	// A host /16 on the range leaves no block.
	full := &net.IPNet{IP: net.ParseIP("10.213.0.5"), Mask: net.CIDRMask(16, 32)}
	if _, ok := firstFreeSubnet(usedSubnets(nil, []net.Addr{full})); ok {
		t.Error("a host /16 on the range should leave no block")
	}
}

func TestCreateInFreeSubnet(t *testing.T) {
	// Full range: plain error, create never called.
	called := false
	err := createInFreeSubnet(prefixes("10.213.0.0/16"), func(netip.Prefix) error { called = true; return nil })
	if !errors.Is(err, errNoSubnet) || called {
		t.Fatalf("full range: err=%v called=%v", err, called)
	}
	if want := "no free network space in 10.213.0.0/16 for a new environment; remove one, or move the host off that range"; err.Error() != want {
		t.Errorf("message = %q", err)
	}

	// Refused overlaps move on to the next block.
	var tried []string
	overlap := errors.New("Error response from daemon: invalid pool request: Pool overlaps with other one on this address space")
	err = createInFreeSubnet(nil, func(p netip.Prefix) error {
		tried = append(tried, p.String())
		if len(tried) < 3 {
			return overlap
		}
		return nil
	})
	if err != nil || len(tried) != 3 || tried[2] != "10.213.2.0/24" {
		t.Fatalf("retry: err=%v tried=%v", err, tried)
	}

	// Other errors stop at once.
	n := 0
	boom := errors.New("daemon down")
	if err = createInFreeSubnet(nil, func(netip.Prefix) error { n++; return boom }); !errors.Is(err, boom) || n != 1 {
		t.Errorf("hard error: err=%v n=%d", err, n)
	}

	// Endless overlaps give up after subnetTries.
	n = 0
	if err = createInFreeSubnet(nil, func(netip.Prefix) error { n++; return overlap }); !errors.Is(err, overlap) || n != subnetTries {
		t.Errorf("give up: err=%v n=%d", err, n)
	}
}

func TestIsOverlap(t *testing.T) {
	for msg, want := range map[string]bool{
		"Error response from daemon: invalid pool request: Pool overlaps with other one on this address space": true,
		"Error response from daemon: networks have overlapping IPv4":                                           true,
		"failed to allocate gateway (10.213.3.1): Address already in use":                                      true,
		"Error response from daemon: all predefined address pools have been fully subnetted":                   false,
		"permission denied": false,
	} {
		if got := isOverlap(errors.New(msg)); got != want {
			t.Errorf("isOverlap(%q) = %v", msg, got)
		}
	}
}

// fakeDaemon answers the two calls EnsureNetwork makes. create decides each
// network create's reply; the subnets it was offered are recorded.
func fakeDaemon(t *testing.T, list []network.Summary, create func(n int) (int, string)) (*Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var offered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/networks"):
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/networks/create"):
			var body network.CreateOptions
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			offered = append(offered, body.IPAM.Config[0].Subnet)
			n := len(offered)
			mu.Unlock()
			code, msg := create(n)
			w.WriteHeader(code)
			if code >= 400 {
				_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
				return
			}
			_, _ = w.Write([]byte(`{"Id":"abc"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &Client{cli: cli}, &offered
}

// firstHostFree is the block EnsureNetwork should pick on an otherwise empty
// daemon, whatever this machine's interfaces hold.
func firstHostFree(t *testing.T, used ...netip.Prefix) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	p, ok := firstFreeSubnet(append(usedSubnets(nil, addrs), used...))
	if !ok {
		t.Skip("host interfaces cover the whole range")
	}
	return p.String()
}

func TestEnsureNetwork(t *testing.T) {
	ctx := context.Background()

	// A free range: the first block goes in the create's IPAM config.
	cli, offered := fakeDaemon(t, nil, func(int) (int, string) { return http.StatusCreated, "" })
	if err := cli.EnsureNetwork(ctx, "env-a", nil); err != nil {
		t.Fatal(err)
	}
	if want := firstHostFree(t); len(*offered) != 1 || (*offered)[0] != want {
		t.Errorf("offered %v, want [%s]", *offered, want)
	}

	// Docker's subnets are skipped, and a refused overlap (403 with a JSON
	// message, as the daemon sends it) moves to the next block.
	taken := []network.Summary{{Name: "other", IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.213.0.0/24"}}}}}
	cli, offered = fakeDaemon(t, taken, func(n int) (int, string) {
		if n == 1 {
			return http.StatusForbidden, "invalid pool request: Pool overlaps with other one on this address space"
		}
		return http.StatusCreated, ""
	})
	if err := cli.EnsureNetwork(ctx, "env-a", nil); err != nil {
		t.Fatal(err)
	}
	if len(*offered) != 2 || (*offered)[0] == (*offered)[1] || (*offered)[0] == "10.213.0.0/24" {
		t.Errorf("offered %v", *offered)
	}

	// A network of that exact name exists: nothing is created. A longer name
	// containing it does not count.
	cli, offered = fakeDaemon(t, []network.Summary{{Name: "env-a"}}, func(int) (int, string) { return http.StatusCreated, "" })
	if err := cli.EnsureNetwork(ctx, "env-a", nil); err != nil || len(*offered) != 0 {
		t.Errorf("existing: err=%v offered=%v", err, *offered)
	}
	cli, offered = fakeDaemon(t, []network.Summary{{Name: "env-a-2"}}, func(int) (int, string) { return http.StatusCreated, "" })
	if err := cli.EnsureNetwork(ctx, "env-a", nil); err != nil || len(*offered) != 1 {
		t.Errorf("substring: err=%v offered=%v", err, *offered)
	}

	// A concurrent create of the same name is success.
	cli, _ = fakeDaemon(t, nil, func(int) (int, string) { return http.StatusConflict, "network with name env-a already exists" })
	if err := cli.EnsureNetwork(ctx, "env-a", nil); err != nil {
		t.Errorf("already exists: %v", err)
	}

	// A full range is the plain error and creates nothing.
	full := []network.Summary{{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.213.0.0/16"}}}}}
	cli, offered = fakeDaemon(t, full, func(int) (int, string) { return http.StatusCreated, "" })
	if err := cli.EnsureNetwork(ctx, "env-a", nil); !errors.Is(err, errNoSubnet) || len(*offered) != 0 {
		t.Errorf("full: err=%v offered=%v", err, *offered)
	}
}
