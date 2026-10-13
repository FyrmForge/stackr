package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
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

var rng213 = netip.MustParsePrefix("10.213.0.0/16")

func TestFirstFree(t *testing.T) {
	for name, tc := range map[string]struct {
		rng  string
		used []string
		want string
	}{
		"empty":         {"10.213.0.0/16", nil, "10.213.0.0/24"},
		"skips used":    {"10.213.0.0/16", []string{"10.213.0.0/24", "10.213.1.0/24"}, "10.213.2.0/24"},
		"wider overlap": {"10.213.0.0/16", []string{"10.213.0.0/23"}, "10.213.2.0/24"},
		"host address":  {"10.213.0.0/16", []string{"10.213.0.7/32"}, "10.213.1.0/24"},
		"gap is reused": {"10.213.0.0/16", []string{"10.213.0.0/24", "10.213.2.0/24"}, "10.213.1.0/24"},
		"foreign nets":  {"10.213.0.0/16", []string{"172.17.0.0/16", "10.0.0.0/24"}, "10.213.0.0/24"},
		"ipv6 ignored":  {"10.213.0.0/16", []string{"fd00::/64"}, "10.213.0.0/24"},
		"last block": {"10.213.0.0/16", []string{
			"10.213.0.0/17", "10.213.128.0/18", "10.213.192.0/19", "10.213.224.0/20",
			"10.213.240.0/21", "10.213.248.0/22", "10.213.252.0/23", "10.213.254.0/24",
		}, "10.213.255.0/24"},
		// A range other than 10.213: a worker's own /16, and the whole supernet.
		"worker range":  {"10.209.0.0/16", []string{"10.209.0.0/24", "10.213.0.0/24"}, "10.209.1.0/24"},
		"other node":    {"10.209.0.0/16", []string{"10.213.0.0/16"}, "10.209.0.0/24"},
		"supernet":      {"10.208.0.0/12", []string{"10.208.0.0/16", "10.209.0.0/16"}, "10.210.0.0/24"},
		"unaligned arg": {"10.209.7.9/16", nil, "10.209.0.0/24"},
	} {
		got, ok := FirstFree(netip.MustParsePrefix(tc.rng), prefixes(tc.used...))
		if !ok || got.String() != tc.want {
			t.Errorf("%s: got %v %v, want %s", name, got, ok, tc.want)
		}
	}
	if _, ok := FirstFree(netip.MustParsePrefix("10.209.0.0/16"), prefixes("10.209.0.0/16")); ok {
		t.Error("a full range yielded a block")
	}
	if _, ok := FirstFree(netip.MustParsePrefix("10.209.0.0/25"), nil); ok {
		t.Error("a range narrower than a network yielded a block")
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
	if _, ok := FirstFree(rng213, usedSubnets(nil, []net.Addr{full})); ok {
		t.Error("a host /16 on the range should leave no block")
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

func TestEnsureNetwork(t *testing.T) {
	ctx := context.Background()
	sub := netip.MustParsePrefix("10.209.4.0/24")
	created := func(int) (int, string) { return http.StatusCreated, "" }

	// The given subnet, and only it, goes in the create's IPAM config.
	cli, offered := fakeDaemon(t, nil, created)
	if err := cli.EnsureNetwork(ctx, "env-a", sub, nil); err != nil {
		t.Fatal(err)
	}
	if len(*offered) != 1 || (*offered)[0] != "10.209.4.0/24" {
		t.Errorf("offered %v", *offered)
	}

	// No subnet and no such network: Docker is never asked to pick one.
	cli, offered = fakeDaemon(t, nil, created)
	if err := cli.EnsureNetwork(ctx, "env-a", netip.Prefix{}, nil); !errors.Is(err, ErrNeedsSubnet) || len(*offered) != 0 {
		t.Errorf("no subnet: err=%v offered=%v", err, *offered)
	}

	// A refused overlap (403 with a JSON message, as the daemon sends it) is
	// ErrSubnetTaken, so the caller reserves another block.
	cli, _ = fakeDaemon(t, nil, func(int) (int, string) {
		return http.StatusForbidden, "invalid pool request: Pool overlaps with other one on this address space"
	})
	if err := cli.EnsureNetwork(ctx, "env-a", sub, nil); !errors.Is(err, ErrSubnetTaken) {
		t.Errorf("overlap: %v", err)
	}

	// A network of that exact name exists: nothing is created, with or
	// without a subnet. A longer name containing it does not count.
	cli, offered = fakeDaemon(t, []network.Summary{{Name: "env-a"}}, created)
	if err := cli.EnsureNetwork(ctx, "env-a", netip.Prefix{}, nil); err != nil || len(*offered) != 0 {
		t.Errorf("existing: err=%v offered=%v", err, *offered)
	}
	cli, offered = fakeDaemon(t, []network.Summary{{Name: "env-a-2"}}, created)
	if err := cli.EnsureNetwork(ctx, "env-a", sub, nil); err != nil || len(*offered) != 1 {
		t.Errorf("substring: err=%v offered=%v", err, *offered)
	}

	// A concurrent create of the same name is success.
	cli, _ = fakeDaemon(t, nil, func(int) (int, string) { return http.StatusConflict, "network with name env-a already exists" })
	if err := cli.EnsureNetwork(ctx, "env-a", sub, nil); err != nil {
		t.Errorf("already exists: %v", err)
	}
}

func TestUsedSubnetsFromDaemon(t *testing.T) {
	cli, _ := fakeDaemon(t, []network.Summary{
		{IPAM: network.IPAM{Config: []network.IPAMConfig{{Subnet: "10.209.4.0/24"}}}},
	}, func(int) (int, string) { return http.StatusCreated, "" })
	used, err := cli.UsedSubnets(context.Background())
	if err != nil || !slices.Contains(used, netip.MustParsePrefix("10.209.4.0/24")) {
		t.Fatalf("used = %v, %v", used, err)
	}
	// A worker's Docker: only its networks, never this host's interfaces
	// (those made every worker "own" the controller's bridges).
	cli.remote = true
	used, err = cli.UsedSubnets(context.Background())
	if err != nil || len(used) != 1 {
		t.Fatalf("remote used = %v, %v; want the daemon's one network", used, err)
	}
}

// netFake is a Networker over a map, enough to drive EnsureReserved.
type netFake struct {
	nets     map[string]netip.Prefix
	used     []netip.Prefix
	taken    map[string]bool // subnets Docker refuses though used does not list them
	attempts []string
}

func (f *netFake) EnsureNetwork(_ context.Context, name string, sub netip.Prefix, _ map[string]string) error {
	if _, ok := f.nets[name]; ok {
		return nil
	}
	if !sub.IsValid() {
		return ErrNeedsSubnet
	}
	f.attempts = append(f.attempts, sub.String())
	if f.taken[sub.String()] {
		return ErrSubnetTaken
	}
	f.nets[name] = sub
	return nil
}

func (f *netFake) UsedSubnets(context.Context) ([]netip.Prefix, error) { return f.used, nil }

// pickFrom reserves from one node's range the way leaf/node does.
type pickFrom map[string]netip.Prefix

func (p pickFrom) Reserve(_ context.Context, node string, used []netip.Prefix) (netip.Prefix, error) {
	b, ok := FirstFree(p[node], used)
	if !ok {
		return b, errors.New("full")
	}
	return b, nil
}

func TestEnsureReserved(t *testing.T) {
	ranges := pickFrom{"w1": netip.MustParsePrefix("10.209.0.0/16")}
	ctx := WithNode(context.Background(), "w1")
	f := &netFake{
		nets:  map[string]netip.Prefix{},
		used:  prefixes("10.209.0.0/24"),
		taken: map[string]bool{"10.209.2.0/24": true},
	}

	// Docker's networks, the caller's known blocks and a block Docker refuses
	// are all stepped over; commit sees each offer before the create.
	var committed []string
	p, err := EnsureReserved(ctx, f, ranges, "env-a", nil, prefixes("10.209.1.0/24"), func(p netip.Prefix) error {
		committed = append(committed, p.String())
		return nil
	})
	if err != nil || p.String() != "10.209.3.0/24" || f.nets["env-a"] != p {
		t.Fatalf("p=%v err=%v nets=%v", p, err, f.nets)
	}
	if strings.Join(committed, ",") != "10.209.2.0/24,10.209.3.0/24" {
		t.Errorf("committed %v", committed)
	}

	// Already there: nothing reserved, the zero prefix.
	p, err = EnsureReserved(ctx, f, ranges, "env-a", nil, nil, func(netip.Prefix) error {
		t.Error("commit on an existing network")
		return nil
	})
	if err != nil || p.IsValid() {
		t.Errorf("existing: p=%v err=%v", p, err)
	}

	// No node in the context is an error, not a guess.
	if _, err := EnsureReserved(context.Background(), f, ranges, "env-b", nil, nil, nil); !errors.Is(err, ErrNoNode) {
		t.Errorf("no node: %v", err)
	}

	// Endless refusals give up after subnetTries offers.
	f = &netFake{nets: map[string]netip.Prefix{}, taken: map[string]bool{}}
	for i := range 20 {
		f.taken[netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 209, byte(i), 0}), 24).String()] = true
	}
	if _, err := EnsureReserved(ctx, f, ranges, "env-c", nil, nil, nil); !errors.Is(err, ErrSubnetTaken) ||
		len(f.attempts) != subnetTries {
		t.Errorf("give up: err=%v attempts=%v", err, f.attempts)
	}
}
