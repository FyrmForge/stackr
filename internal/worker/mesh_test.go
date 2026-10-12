package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/wgapply"
)

// meshRunner records every command. An iptables -C check fails, so the rule
// gets added, as on a fresh chain.
type meshRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *meshRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	switch {
	case name == "iptables" && slicesHas(args, "-C"):
		return nil, errors.New("no such rule")
	case name == "ip" && len(args) > 1 && args[1] == "route" && args[0] == "-o":
		return []byte("1.1.1.1 via 10.0.0.1 dev eth0 src 10.0.0.5"), nil
	}
	return nil, nil
}

func slicesHas(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func (r *meshRunner) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

type pingFake struct {
	mu   sync.Mutex
	down map[string]bool // by iface
}

func (p *pingFake) Ping(_ context.Context, r exits.Rung) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down[r.Iface] {
		return errors.New("timeout")
	}
	return nil
}

func meshSpec() MeshSpec {
	iface := exits.Iface("nx")
	return MeshSpec{
		Address:    netip.MustParsePrefix("10.207.255.1/24"),
		ListenPort: 51820,
		Peers: []mesh.Peer{{NodeID: "nx", PublicKey: "pk",
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.209.0.0/16")},
			Candidates: []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:51820")}}},
		Links: []exits.Link{{Iface: iface, Port: 52001, PeerKey: "xk", Endpoint: "10.207.255.2:52001",
			Addr: "10.251.0.1/30", Table: 1001, Sources: []string{"10.213.1.0/24"}}},
		Serves: []exits.Serve{{Iface: exits.Iface("nc"), Port: 52016, PeerKey: "ck",
			Addr: "10.251.0.6/30", PeerAddr: "10.251.0.5/32", Subnets: []string{"10.213.9.0/24"},
			Table: 1100, Mark: 0x5000, Pref: 20000}},
		Ladders: []LadderSpec{{
			Subject: exits.Subject{Sources: []string{"10.213.1.0/24"}, PrefBase: 10000},
			Rungs: []exits.Rung{
				{NodeID: "nx", Iface: iface, Table: 1001, Target: "10.251.0.2"},
				{NodeID: "self", Own: true, Target: "1.1.1.1"},
			},
		}},
	}
}

func newMeshServer(t *testing.T, dir string, p *pingFake) (*Server, *meshRunner, *wgapply.Recorder) {
	t.Helper()
	run, rec := &meshRunner{}, &wgapply.Recorder{}
	s := New(Config{StateDir: dir, Runner: run, DockerUserPoll: time.Millisecond,
		Mesh: MeshConfig{WG: rec, Pinger: p, LadderEvery: time.Hour, FailoverEvery: time.Hour, ReassertEvery: time.Hour}})
	t.Cleanup(s.Stop)
	return s, run, rec
}

func TestApplyMeshAndReplay(t *testing.T) {
	dir := t.TempDir()
	ping := &pingFake{}
	s, run, rec := newMeshServer(t, dir, ping)
	ctx := context.Background()
	if err := s.ApplyMesh(ctx, meshSpec()); err != nil {
		t.Fatal(err)
	}
	cfg, ok := rec.Last()
	if !ok || cfg.PrivateKey == "" || cfg.Iface != mesh.Iface || len(cfg.Peers) != 1 {
		t.Fatalf("wg config = %+v", cfg)
	}
	for _, want := range []string{
		"ip link add " + exits.Iface("nx") + " type wireguard", // client link
		"ip link add " + exits.Iface("nc") + " type wireguard", // exit side
		"-I STACKR-NAT 1 -o " + exits.Iface("nx"),
		"-A STACKR-XM-" + exits.Iface("nc"),
		"-o eth0 -j MASQUERADE", // uplink found from the route
	} {
		if run.count(want) == 0 {
			t.Errorf("no call containing %q in\n%s", want, strings.Join(run.calls, "\n"))
		}
	}
	// First tick done before ApplyMesh returned: the exit rung is routed.
	if run.count("ip rule add pref 10001 from 10.213.1.0/24 lookup 1001") != 1 {
		t.Errorf("ladder not routed over the exit:\n%s", strings.Join(run.calls, "\n"))
	}
	info, err := s.MeshInfo()
	if err != nil || info.WGPublic == "" || info.ExitPublic == "" || info.WGPublic == info.ExitPublic {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if len(info.Ladders) != 1 || info.Ladders[0].State.NodeID != "nx" {
		t.Errorf("ladders = %+v", info.Ladders)
	}

	// The same push again changes nothing on the node.
	links := run.count("ip link add")
	if err := s.ApplyMesh(ctx, meshSpec()); err != nil {
		t.Fatal(err)
	}
	if run.count("ip link add") != links {
		t.Error("an unchanged push rebuilt a link")
	}

	// A new process on the same state dir restores wg, links and ladders.
	s.Stop()
	s2, run2, rec2 := newMeshServer(t, dir, &pingFake{})
	if err := s2.replayState(ctx); err != nil {
		t.Fatal(err)
	}
	cfg2, ok := rec2.Last()
	if !ok || cfg2.PrivateKey != cfg.PrivateKey {
		t.Errorf("replayed wg config = %+v", cfg2)
	}
	if run2.count("ip link add "+exits.Iface("nx")) != 1 || run2.count("ip link add "+exits.Iface("nc")) != 1 {
		t.Errorf("links not restored:\n%s", strings.Join(run2.calls, "\n"))
	}
	if run2.count("ip rule add pref 10001") != 1 {
		t.Errorf("ladder not restored:\n%s", strings.Join(run2.calls, "\n"))
	}
}

func TestMeshFailsOverAndClears(t *testing.T) {
	ping := &pingFake{down: map[string]bool{exits.Iface("nx"): true}}
	s, run, _ := newMeshServer(t, t.TempDir(), ping)
	ctx := context.Background()
	if err := s.ApplyMesh(ctx, meshSpec()); err != nil {
		t.Fatal(err)
	}
	// The exit is down at the first tick, so the own uplink (second rung) is
	// live: the subject's rules are only removed, never added.
	if run.count("ip rule add pref 10001") != 0 {
		t.Errorf("routed over a dead exit:\n%s", strings.Join(run.calls, "\n"))
	}
	if st := mustInfo(t, s).Ladders[0].State; st.NodeID != "self" {
		t.Errorf("live rung = %+v", st)
	}

	// Dropping the ladder and the links clears everything.
	if err := s.ApplyMesh(ctx, MeshSpec{Address: meshSpec().Address, ListenPort: 51820}); err != nil {
		t.Fatal(err)
	}
	if run.count("ip link del "+exits.Iface("nx")) < 2 { // Up's quiet del, then Down
		t.Errorf("link not removed:\n%s", strings.Join(run.calls, "\n"))
	}
	if run.count("ip rule del pref 10000") == 0 {
		t.Error("ladder rules not cleared")
	}
	if len(mustInfo(t, s).Ladders) != 0 {
		t.Error("ladder still running")
	}
}

func mustInfo(t *testing.T, s *Server) MeshInfo {
	t.Helper()
	i, err := s.MeshInfo()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestIPTablesPushReassertsExitRules(t *testing.T) {
	s, run, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	if err := s.ApplyMesh(context.Background(), meshSpec()); err != nil {
		t.Fatal(err)
	}
	before, links := run.count("-I STACKR-NAT 1 -o "+exits.Iface("nx")), run.count("ip link add")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/iptables", "text/plain", strings.NewReader("*nat\n:STACKR-NAT - [0:0]\nCOMMIT\n"))
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("push = %v, %v", resp, err)
	}
	if run.count("-I STACKR-NAT 1 -o "+exits.Iface("nx")) != before+1 {
		t.Error("exit rule not added back after the chain was redeclared")
	}
	if run.count("ip link add") != links {
		t.Error("reassert rebuilt a link")
	}
}

func TestMeshEndpoint(t *testing.T) {
	s, _, _ := newMeshServer(t, t.TempDir(), &pingFake{})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	b, _ := json.Marshal(meshSpec())
	resp, err := http.Post(srv.URL+"/v1/mesh", "application/json", bytes.NewReader(b))
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("POST /v1/mesh = %v, %v", resp, err)
	}
	var info MeshInfo
	resp, err = http.Get(srv.URL + "/v1/mesh")
	if err != nil || json.NewDecoder(resp.Body).Decode(&info) != nil || info.WGPublic == "" {
		t.Fatalf("GET /v1/mesh = %+v, %v", info, err)
	}

	// No applier: 501, like /v1/wg.
	bare := httptest.NewServer(New(Config{StateDir: t.TempDir(), Runner: &meshRunner{}}).Handler())
	defer bare.Close()
	resp, _ = http.Post(bare.URL+"/v1/mesh", "application/json", bytes.NewReader(b))
	if resp.StatusCode != 501 {
		t.Errorf("without wg = %d, want 501", resp.StatusCode)
	}
}
