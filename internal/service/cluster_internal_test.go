package service

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func facts(ifs ...worker.Iface) *worker.Facts {
	return &worker.Facts{Interfaces: ifs, Docker: "29.9", Arch: "amd64"}
}

func iface(name string, public bool, prefixes ...string) worker.Iface {
	return worker.Iface{Name: name, Public: public, Prefixes: prefixes}
}

// Four nodes: a home LAN of two behind a router, and a cloud network of two
// where one has the public address. The cloud hands out /32s and a route
// (Hetzner), so the network is the route's.
func TestBuildClusterMap(t *testing.T) {
	node := func(id, role, transit string, f *worker.Facts, ping map[string]worker.PeerPing) clusterNodeIn {
		f.Ping = ping
		return clusterNodeIn{
			Node:  store.Node{ID: id, Name: id, Role: role, Tags: `["ingress"]`, Status: "online", WGAddress: transit},
			Facts: f, Up: true,
		}
	}
	// Latency is the pinged round trip, the lower of the two ends; a pair
	// nobody got a reply for has none (fsn1 lost every probe to nas).
	m := buildClusterMap([]clusterNodeIn{
		node("home", "controller", "", facts(
			iface("eth0", false, "192.168.1.10/24"),
			iface("docker0", false, "172.17.0.1/16"),
			iface("wg0", false, "10.207.255.1/24"),
		), map[string]worker.PeerPing{"10.207.255.1": {RTTms: 0.4}, "10.207.255.2": {RTTms: 21}, "10.207.255.3": {RTTms: 25}}),
		node("nas", "worker", "10.207.255.1", facts(iface("eth0", false, "192.168.1.20/24")),
			map[string]worker.PeerPing{"10.207.255.6": {RTTms: 0.3}}),
		node("fsn1", "worker", "10.207.255.2", facts(
			iface("eth0", true, "203.0.113.5/32"),
			worker.Iface{Name: "enp7s0", Prefixes: []string{"10.20.0.2/32"}, Routes: []string{"10.20.0.0/16", "10.20.0.1/32"}},
		), map[string]worker.PeerPing{"10.207.255.1": {Loss: 1}, "10.207.255.3": {RTTms: 0.8}}),
		node("fsn2", "worker", "10.207.255.3", facts(
			worker.Iface{Name: "enp7s0", Prefixes: []string{"10.20.0.3/32"}, Routes: []string{"10.20.0.0/16"}},
		), map[string]worker.PeerPing{"10.207.255.6": {RTTms: 23}}),
	})
	if len(m.Networks) != 2 {
		t.Fatalf("networks = %+v, want the LAN and the cloud network", m.Networks)
	}
	if home := m.Networks[0]; home.CIDR != "192.168.1.0/24" || !home.Router || len(home.Nodes) != 2 {
		t.Errorf("first network = %+v, want the controller's LAN behind a router", home)
	}
	if cloud := m.Networks[1]; cloud.CIDR != "10.20.0.0/16" || cloud.Router {
		t.Errorf("second network = %+v, want 10.20.0.0/16 with a public node, no router", cloud)
	}
	if got := m.Nodes[2].Public; len(got) != 1 || got[0] != "203.0.113.5" {
		t.Errorf("fsn1 public = %v", got)
	}
	if m.Nodes[0].Docker != "29.9" || m.Nodes[0].Arch != "amd64" {
		t.Errorf("controller docker %q arch %q, want 29.9 amd64", m.Nodes[0].Docker, m.Nodes[0].Arch)
	}
	if len(m.Links) != 6 {
		t.Fatalf("links = %d, want one per pair (6)", len(m.Links))
	}
	want := map[string]struct {
		kind string
		ms   float64
	}{
		"home-nas":  {LinkLAN, 0.3},
		"home-fsn1": {LinkInternet, 21},
		"home-fsn2": {LinkInternet, 23},
		"nas-fsn1":  {LinkInternet, -1},
		"nas-fsn2":  {LinkInternet, -1},
		"fsn1-fsn2": {LinkPrivate, 0.8},
	}
	for _, l := range m.Links {
		w := want[l.A+"-"+l.B]
		if l.Kind != w.kind || l.MS != w.ms {
			t.Errorf("%s-%s = %s %.1f ms, want %s %.1f", l.A, l.B, l.Kind, l.MS, w.kind, w.ms)
		}
	}
}

// A node that went quiet keeps its docker and arch from memory.
func TestBuildClusterMapMemoryArch(t *testing.T) {
	m := buildClusterMap([]clusterNodeIn{{
		Node: store.Node{ID: "w", Name: "w", Role: "worker", Status: "online"},
		Mem:  &nodeMemory{Net: "10.0.0.0/24", Docker: "28.1", Arch: "arm64"},
	}})
	if n := m.Nodes[0]; n.Docker != "28.1" || n.Arch != "arm64" {
		t.Errorf("remembered docker %q arch %q", n.Docker, n.Arch)
	}
}

// A worker that stopped answering has no tunnel up and says so; one that
// never reported sits in the "unknown" box.
func TestBuildClusterMapUnreachable(t *testing.T) {
	n := func(id, role string, f *worker.Facts, up bool) clusterNodeIn {
		return clusterNodeIn{Node: store.Node{ID: id, Name: id, Role: role, Status: "online"}, Facts: f, Up: up}
	}
	m := buildClusterMap([]clusterNodeIn{
		n("c", "controller", facts(iface("eth0", true, "198.51.100.1/32")), true),
		n("w", "worker", facts(iface("eth0", true, "198.51.100.2/32")), false),
		n("x", "worker", nil, true),
	})
	if m.Nodes[1].Status != NodeUnreachable {
		t.Errorf("status = %q, want unreachable", m.Nodes[1].Status)
	}
	if len(m.Links) != 1 || m.Links[0].A != "c" || m.Links[0].B != "x" {
		t.Errorf("links = %+v, want only the pair that is online", m.Links)
	}
	if last := m.Networks[len(m.Networks)-1]; last.ID != "unknown" || last.Router {
		t.Errorf("last network = %+v, want the unknown box without a router", last)
	}
	w := n("w", "worker", facts(iface("eth0", true, "198.51.100.2/32")), false)
	w.Alive = true
	if s := buildClusterMap([]clusterNodeIn{w}).Nodes[0].Status; s != NodeTunnelDown {
		t.Errorf("tunnel missed, liveness answered: status = %q, want %q", s, NodeTunnelDown)
	}
}

// Two ingress nodes behind one NAT: the router forwards to home, so nas is
// not reachable. Unsure and nobody answering makes no claim.
func TestApplyProbes(t *testing.T) {
	m := ClusterMap{
		Nodes: []ClusterNode{
			{ID: "home", Net: "lan", Tags: []string{"ingress"}},
			{ID: "nas", Net: "lan", Tags: []string{"ingress"}},
			{ID: "pi", Net: "lan2", Tags: []string{"ingress"}},
		},
		Networks: []ClusterNet{{ID: "lan", Router: true}, {ID: "lan2", Router: true}},
	}
	applyProbes(&m, map[string]string{"home": "home", "nas": "home", "pi": ""}, map[string]bool{"lan2": true})
	if m.Networks[0].Forward != "home" || m.Networks[0].Unsure {
		t.Errorf("lan = %+v, want forwarding to home, sure", m.Networks[0])
	}
	if m.Nodes[0].NoForward || !m.Nodes[1].NoForward {
		t.Errorf("home %v nas %v, want only nas unreachable", m.Nodes[0].NoForward, m.Nodes[1].NoForward)
	}
	if m.Nodes[2].NoForward || m.Networks[1].Forward != "" {
		t.Errorf("an unsure probe nobody answered claimed %+v %+v", m.Nodes[2], m.Networks[1])
	}
}

func TestProbePath(t *testing.T) {
	if p := probePath("ctl"); !strings.HasPrefix(p, worker.ProbePrefix) || len(p) != len(worker.ProbePrefix)+24 || p == probePath("other") {
		t.Errorf("probePath = %q", p)
	}
}

// The map card's cert note: expired, or the days left under 14; a renewed
// cert, a revoked one and no certs at all say nothing.
func TestClusterCertNote(t *testing.T) {
	now := time.Now()
	cert := func(d time.Duration, revoked bool) store.NodeCert {
		c := store.NodeCert{NotAfter: now.Add(d)}
		if revoked {
			c.RevokedAt = &now
		}
		return c
	}
	for _, c := range []struct {
		name    string
		certs   []store.NodeCert
		expired bool
		days    int
	}{
		{"none", nil, false, 0},
		{"fresh", []store.NodeCert{cert(60*24*time.Hour, false)}, false, 0},
		{"nine days", []store.NodeCert{cert(9*24*time.Hour-time.Hour, false)}, false, 9},
		{"expired", []store.NodeCert{cert(-48*time.Hour, false)}, true, 0},
		{"renewed", []store.NodeCert{cert(-48*time.Hour, false), cert(60*24*time.Hour, false)}, false, 0},
		{"revoked fresh does not count", []store.NodeCert{cert(-48*time.Hour, false), cert(60*24*time.Hour, true)}, true, 0},
	} {
		expired, days := clusterCertNote(c.certs, now)
		if expired != c.expired || days != c.days {
			t.Errorf("%s: got %v, %d; want %v, %d", c.name, expired, days, c.expired, c.days)
		}
	}
	n := store.Node{ID: "w", Name: "w", Role: "worker", Status: "online"}
	m := buildClusterMap([]clusterNodeIn{{Node: n, Up: true, Behind: true, CertDays: 9}})
	if g := m.Nodes[0]; !g.Behind || g.CertDays != 9 || g.CertExpired {
		t.Fatalf("card = %+v", g)
	}
}
