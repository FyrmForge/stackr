package admin

import (
	"strings"
	"testing"
)

// The network tab groups addresses and labels them: a cluster row carries its
// env path, and hidden rows sit in a native details with their label.
func TestNodeNetworkTab(t *testing.T) {
	var b strings.Builder
	err := NodeBody("network", NodeView{Ifaces: []IfaceRow{
		{Group: "public", Name: "eth0", Addr: "203.0.113.5/24"},
		{Group: "cluster", Name: "br-1a2b", Addr: "10.208.3.1/24", Label: "acme/shop/prod"},
		{Group: "hidden", Name: "eth0", Addr: "fe80::1/64", Label: "link-local"},
	}}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	d := strings.Index(out, "<details")
	for _, want := range []string{"Public", "Cluster", "acme/shop/prod", "203.0.113.5/24"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if d < 0 || !strings.Contains(out[d:], "link-local") || strings.Contains(out[:d], "link-local") {
		t.Errorf("link-local not inside details\n%s", out)
	}
}

// A tunnel row opens the tunnel drawer in the drawer body without touching the URL.
func TestNodeOverviewTunnelRow(t *testing.T) {
	var b strings.Builder
	err := NodeBody("overview", NodeView{Status: "online", Cores: 1, HasFacts: true, Tunnels: []TunnelRow{
		{Name: "h4", Dials: "h4 dials", Ping: "12 ms", Open: "/-/cluster/tunnel?a=a&b=b"},
	}}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 core", `hx-get="/-/cluster/tunnel?a=a&amp;b=b"`, `hx-target="#drawer-body"`, `hx-push-url="false"`, "h4 dials"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}

// The Update tab lists each worker; after a stop it says why and offers Retry.
func TestUpdateWorkers(t *testing.T) {
	var b strings.Builder
	err := Update(UpdateView{Version: "1.0", Workers: &WorkersView{
		Stopped: "h3 failed to upgrade, rolled back", Action: "/-/admin/update/workers", Button: "Retry",
		Rows: []WorkerRow{
			{Name: "h3", Current: "0.9", State: "failed", Error: "health check timed out", Behind: true},
			{Name: "h4", Current: "1.0", State: "done"},
		},
	}}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"h3 failed to upgrade, rolled back", "failed", "health check timed out", "done", "Retry", `hx-post="/-/admin/update/workers"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}
