package domain_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

// One node, nothing named: the per-node config is the old single config.
func TestBuildNodeSingleNodeIdentical(t *testing.T) {
	in := install
	in.PanelHost, in.PanelUpstream = "panel.example.com", "stackrd:8080"
	tiles := one(row("app.example.com", domain.Extras{}))
	ext := []domain.Route{{Host: "x.example.com", Mode: "http", Target: "10.0.0.5:80"}}
	want, err := domain.Build(in, tiles, ext, nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := domain.BuildAll(in, tiles, []domain.NodeRoute{{Route: ext[0]}}, nil)
	if err != nil || len(all) != 1 || !bytes.Equal(all[""], want) {
		t.Fatalf("single node differs: %v\n%s\n%s", err, all[""], want)
	}
	in.DefaultIngress = "n1"
	all, _ = domain.BuildAll(in, tiles, []domain.NodeRoute{{Route: ext[0]}}, nil)
	if len(all) != 1 || !bytes.Equal(all["n1"], want) {
		t.Fatal("a named default ingress node changed the config")
	}
}

func TestBuildAllPerIngressNode(t *testing.T) {
	in := install
	in.PanelHost, in.PanelUpstream, in.DefaultIngress = "panel.example.com", "stackrd:8080", "head"
	local := one(row("local.example.com", domain.Extras{}))[0]
	local.IngressNode, local.Node = "head", "head"
	remote := one(row("far.example.com", domain.Extras{}))[0]
	remote.IngressNode, remote.Node, remote.VIP = "edge", "w2", "10.9.0.7"
	def := one(row("def.example.com", domain.Extras{}))[0] // no ingress node: the default
	all, err := domain.BuildAll(in, []domain.TileRoute{local, remote, def},
		[]domain.NodeRoute{{Route: domain.Route{Host: "r.example.com", Mode: "http", Target: "1.2.3.4:80"}, IngressNode: "edge"}}, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("%v %d", err, len(all))
	}
	head, edge := string(all["head"]), string(all["edge"])
	for _, h := range []string{"panel.example.com", "local.example.com", "def.example.com"} {
		if !strings.Contains(head, h) || strings.Contains(edge, h) {
			t.Errorf("%s belongs on head only", h)
		}
	}
	for _, h := range []string{"far.example.com", "r.example.com"} {
		if !strings.Contains(edge, h) || strings.Contains(head, h) {
			t.Errorf("%s belongs on edge only", h)
		}
	}
	if !strings.Contains(edge, `"10.9.0.7:8080"`) || strings.Contains(edge, "web-r1") {
		t.Errorf("cross-node upstream should be the VIP: %s", edge)
	}
	if !strings.Contains(head, `"web-r1:8080"`) {
		t.Errorf("same-node upstream should stay container names: %s", head)
	}
}

// Trusted proxies are the controller's front proxies: a worker's Caddy
// never trusts them.
func TestBuildAllTrustedProxiesOnControllerOnly(t *testing.T) {
	in := install
	in.DefaultIngress, in.Controller = "ctl", "ctl"
	in.TrustedProxies = []string{"192.168.1.100/32"}
	in.IngressNodes = []string{"w1"}
	all, err := domain.BuildAll(in, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(all["ctl"]), "192.168.1.100/32") {
		t.Error("the controller lost its trusted proxy")
	}
	if strings.Contains(string(all["w1"]), "192.168.1.100/32") {
		t.Error("a worker trusts the controller's front proxy")
	}
}

// Every ingress node solves DNS-01 the same way, through stackrd (a worker's
// stackrd worker passes the call to the controller), with no token anywhere.
func TestBuildAllDNSChallengeOnEveryIngress(t *testing.T) {
	in := install
	in.DefaultIngress, in.Controller, in.Managed = "ctl", "ctl", []string{"example.com"}
	ctl := one(row("*.a.example.com", domain.Extras{}))[0]
	ctl.IngressNode = "ctl"
	w := one(row("*.b.example.com", domain.Extras{}))[0]
	w.IngressNode = "w1"
	all, err := domain.BuildAll(in, []domain.TileRoute{ctl, w}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for n, wild := range map[string]string{"ctl": "*.a.example.com", "w1": "*.b.example.com"} {
		cfg := string(all[n])
		if !strings.Contains(cfg, `"provider":{"name":"stackr"}`) || !strings.Contains(cfg, `"automate":["*.example.com"]`) {
			t.Errorf("%s: no stackr DNS-01 policy: %s", n, cfg)
		}
		if !strings.Contains(cfg, `"subjects":["`+wild+`","*.example.com"]`) {
			t.Errorf("%s: %s is not on the DNS-01 policy: %s", n, wild, cfg)
		}
		if strings.Contains(cfg, "{env.") || strings.Contains(cfg, "token") {
			t.Errorf("%s: a secret in the config", n)
		}
	}
}

// The port-forward probe answers each node's own id, first on both servers.
func TestBuildAllProbe(t *testing.T) {
	in := install
	in.DefaultIngress, in.Probe = "head", "/.well-known/stackr-probe/abc"
	edge := one(row("far.example.com", domain.Extras{}))[0]
	edge.IngressNode = "edge"
	all, err := domain.BuildAll(in, []domain.TileRoute{edge}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for node, cfg := range all {
		probe := `{"handle":[{"body":"` + node + `","handler":"static_response","status_code":200}],"match":[{"path":["/.well-known/stackr-probe/abc"]}],"terminal":true}`
		if got := strings.Count(string(cfg), probe); got != 2 {
			t.Errorf("%s: probe route %d times, want on :80 and :443", node, got)
		}
		if !strings.Contains(string(cfg), `"routes":[`+probe) {
			t.Errorf("%s: the probe is not the first route", node)
		}
	}
}

// A node tagged ingress gets a config (and so its probe) with nothing routed
// to it; a tile on a worker is dialled by VIP even from that worker's Caddy.
func TestBuildAllIngressNodesAndWorkerLocal(t *testing.T) {
	in := install
	in.DefaultIngress, in.IngressNodes, in.Probe = "head", []string{"head", "idle", "w1"}, "/.well-known/stackr-probe/abc"
	local := one(row("w1.example.com", domain.Extras{}))[0]
	local.IngressNode, local.Node, local.VIP = "w1", "w1", "10.210.1.2"
	all, err := domain.BuildAll(in, []domain.TileRoute{local}, nil, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("%v: %d configs, want head, idle and w1", err, len(all))
	}
	if !strings.Contains(string(all["idle"]), `"body":"idle"`) {
		t.Error("idle ingress node has no probe")
	}
	if w1 := string(all["w1"]); !strings.Contains(w1, `"10.210.1.2:8080"`) || strings.Contains(w1, "web-r1") {
		t.Errorf("worker-local upstream should be the VIP: %s", w1)
	}
}
