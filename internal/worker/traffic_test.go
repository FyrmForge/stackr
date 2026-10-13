package worker

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// GET /v1/traffic: only lines with one of this node's tile containers at an
// end leave the node, with those containers' IPs by tile.
func TestTraffic(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	docker := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "stackr.managed") {
			t.Errorf("list not filtered: %s", r.URL.RawQuery)
		}
		if r.URL.Path == "/networks" {
			_, _ = w.Write([]byte(`[{"IPAM":{"Config":[{"Subnet":"10.210.1.0/24","Gateway":"10.210.1.1"}]}},
				{"IPAM":{"Config":[{"Subnet":"10.210.2.0/24"}]}}]`))
			return
		}
		_, _ = w.Write([]byte(`[
			{"Labels":{"stackr.managed":"true","stackr.tile":"app"},
			 "NetworkSettings":{"Networks":{"env":{"IPAddress":"10.210.1.5"},"ing":{"IPAddress":"10.210.2.3"}}}},
			{"Labels":{"stackr.managed":"true"},"NetworkSettings":{"Networks":{"x":{"IPAddress":"10.210.1.9"}}}}]`))
	})}
	go func() { _ = docker.Serve(ln) }()
	defer docker.Close()

	ct := filepath.Join(dir, "ct")
	table := "ipv4 2 tcp 6 1 src=10.210.1.5 dst=1.1.1.1 sport=1 dport=443 bytes=10 src=1.1.1.1 dst=10.210.1.5 bytes=20\n" +
		"ipv4 2 tcp 6 1 src=172.30.0.1 dst=10.210.2.3 sport=2 dport=80 bytes=30 src=10.210.2.3 dst=172.30.0.1 bytes=40\n" +
		"ipv4 2 tcp 6 1 src=10.212.0.3 dst=10.211.0.4 sport=3 dport=80 bytes=50 src=10.211.0.4 dst=10.212.0.3 bytes=60\n" + // forwarded
		"ipv4 2 tcp 6 1 src=10.210.1.9 dst=1.1.1.1 sport=4 dport=80 bytes=70 src=1.1.1.1 dst=10.210.1.9 bytes=80\n" // not a tile
	if err := os.WriteFile(ct, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{DockerSock: sock, Conntrack: ct})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/traffic", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got Traffic
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tiles) != 2 || got.Tiles["10.210.1.5"] != "app" || got.Tiles["10.210.2.3"] != "app" {
		t.Errorf("tiles = %v", got.Tiles)
	}
	if strings.Join(got.Gateways, ",") != "10.210.1.1,10.210.2.1" {
		t.Errorf("gateways = %v, want the set one and the unset one's first host", got.Gateways)
	}
	lines := strings.Split(strings.TrimSpace(got.Lines), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "sport=1 ") || !strings.Contains(lines[1], "sport=2 ") {
		t.Errorf("lines = %q, want the app's two connections only", got.Lines)
	}
}

func TestEnableAcct(t *testing.T) {
	old := acctKnob
	defer func() { acctKnob = old }()
	acctKnob = filepath.Join(t.TempDir(), "acct")
	if err := os.WriteFile(acctKnob, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	enableAcct()
	if b, _ := os.ReadFile(acctKnob); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("acct = %q, want 1", b)
	}
}

// A netlink dump (no procfs file, no conntrack tool) renders in conntrack
// -L's format, so the same filter and parser read it.
func TestNetlinkFlowLine(t *testing.T) {
	f := netlink.ConntrackFlow{FamilyType: netlink.FAMILY_V4}
	f.Forward.Protocol, f.Forward.SrcIP, f.Forward.DstIP = 6, net.ParseIP("10.0.0.9"), net.ParseIP("10.210.0.3")
	f.Forward.SrcPort, f.Forward.DstPort, f.Forward.Bytes = 40000, 80, 512
	f.Reverse.SrcIP, f.Reverse.DstIP, f.Reverse.Bytes = net.ParseIP("10.210.0.3"), net.ParseIP("10.0.0.9"), 2048
	got := localLines([]byte(f.String()+"\n"), map[string]string{"10.210.0.3": "web"})
	if src, dst := origEnds(string(got)); src != "10.0.0.9" || dst != "10.210.0.3" {
		t.Fatalf("kept %q, ends %s %s", got, src, dst)
	}
}
