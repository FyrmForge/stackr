package admin

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
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

// A live tab polls itself into #tab-update (the poller on its own element,
// so no button inherits it) and carries the hidden Reconnecting note main.js
// shows; a settled tab has neither. Pending says why there is no button.
func TestUpdateLive(t *testing.T) {
	render := func(v UpdateView) string {
		var b strings.Builder
		if err := Update(v).Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	out := render(UpdateView{Version: "1.0", Upgrading: true, Live: "/-/admin/update",
		Workers: &WorkersView{Pending: true, Rows: []WorkerRow{{Name: "h3", Current: "0.9", Behind: true}}}})
	for _, want := range []string{
		`data-live`, `<span hidden hx-get="/-/admin/update" hx-trigger="every 3s" hx-select="#tab-update" hx-target="#tab-update" hx-swap="outerHTML">`,
		`data-reconnecting hidden`, "Reconnecting...", "The rollout starts once the panel's image is published.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("live tab: no %q in\n%s", want, out)
		}
	}
	out = render(UpdateView{Version: "1.0", Job: &c.JobStatusView{Kind: "upgrade", State: "running", Live: true, StreamURL: "/-/jobs/j/events", Refresh: "/-/admin/update"}})
	if !strings.Contains(out, "data-live") || !strings.Contains(out, "data-reconnecting") || strings.Contains(out, "every 3s") {
		t.Errorf("job-stream tab: want data-live and the note, no poller\n%s", out)
	}
	out = render(UpdateView{Version: "1.0", Workers: &WorkersView{Rows: []WorkerRow{{Name: "h4", Current: "1.0"}}}})
	for _, not := range []string{"data-live", "hx-trigger", "data-reconnecting"} {
		if strings.Contains(out, not) {
			t.Errorf("settled tab has %q\n%s", not, out)
		}
	}
}

func renderNode(t *testing.T, tab string, v NodeView) string {
	t.Helper()
	var b strings.Builder
	if err := NodeBody(tab, v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The overview says when the build is behind or too old, and puts the
// expired-cert warning with its Rejoin button first; under 14 days it only notes it.
func TestNodeOverviewVersionAndCert(t *testing.T) {
	out := renderNode(t, "overview", NodeView{Status: "online", Version: "0.6.0-dev.12", Behind: true, CertExpired: true, Rejoin: "/-/admin/nodes/n1/rejoin",
		Warnings: []WarningRow{{Detail: "Cert expired, rejoin it.", Critical: true}, {Detail: "Its clock is 2m ahead of the panel."}}})
	for _, want := range []string{"0.6.0-dev.12, behind", "Cert expired, rejoin it.", "Its clock is 2m ahead", `hx-post="/-/admin/nodes/n1/rejoin"`} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "Cert expired") > strings.Index(out, "Status") {
		t.Errorf("the cert warning is not at the top\n%s", out)
	}
	if out := renderNode(t, "overview", NodeView{Status: "online", Version: "0.1.0", TooOld: true, Behind: true}); !strings.Contains(out, "0.1.0, too old") {
		t.Errorf("no too old in\n%s", out)
	}
	out = renderNode(t, "overview", NodeView{Status: "online", Version: "1.0", CertExpiry: "Oct 23, 2026", Warnings: []WarningRow{{Detail: "Its certificate expires in 9 days."}}})
	if !strings.Contains(out, "expires in 9 days") || strings.Contains(out, "/rejoin") {
		t.Errorf("cert days\n%s", out)
	}
	if out := renderNode(t, "overview", NodeView{Status: "online", Version: "1.0", CertExpiry: "Jan 1, 2027"}); strings.Contains(out, "Cert expires in") {
		t.Errorf("a far cert is noted\n%s", out)
	}
}

// The ingress preview lists hosts in the order given (panel first), says where
// each points and asks for the node's name; the form posts to the preview.
func TestIngressPreview(t *testing.T) {
	var b strings.Builder
	err := Traffic(TrafficView{Defaults: &DefaultsView{
		IngressAction: "/-/admin/nodes/default-ingress/preview",
		Preview: &IngressPreview{
			Node: "h4", PanelNote: "The panel moves to h4.", Cancel: "/-/admin/traffic",
			Hosts: []IngressHostRow{
				{Host: "panel.example.com", Points: "points here", OK: true},
				{Host: "app.example.com", Points: "points to 1.2.3.4"},
			},
			Confirm: c.ConfirmView{Button: "Move default ingress", Title: "Move?", Word: "h4", Kept: []string{"apps"}, Action: "/x"},
		},
	}}).Render(t.Context(), &b)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"The panel moves to h4.", "points here", "points to 1.2.3.4", `hx-post="/-/admin/nodes/default-ingress/preview"`, `data-word`} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "panel.example.com") > strings.Index(out, "app.example.com") {
		t.Errorf("the panel host is not first\n%s", out)
	}
}

// The overview's Backups section: the button, then each group with its
// volumes in a native details element.
func TestNodeOverviewBackups(t *testing.T) {
	out := renderNode(t, "overview", NodeView{Status: "online", Backup: "/-/admin/nodes/n1/backup", Backups: []BackupGroupRow{{
		Label: "node h3, 14 Oct", Status: "done", Dest: "offsite", Warning: "No backup destination set; archives stay on the controller.",
		Volumes: []BackupVolumeRow{{Volume: "pgdata", Where: "acme/shop/prod", Method: "dump", Status: "done", Size: "4 MB"}},
	}}})
	d := strings.Index(out, "<details")
	for _, want := range []string{"Back up node", `hx-post="/-/admin/nodes/n1/backup"`, "node h3, 14 Oct", "offsite", "archives stay on the controller", "pgdata"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if d < 0 || !strings.Contains(out[d:], "pgdata") || strings.Contains(out[:d], "pgdata") {
		t.Errorf("volumes not inside details\n%s", out)
	}
}

// A lost env names the placement whose repoint brings it back.
func TestLostEnvNamesPlacement(t *testing.T) {
	var b strings.Builder
	if err := Nodes(NodesView{Lost: []LostEnvRow{{Path: "acme/shop/prod", Node: "h3", Placement: "eu-db", Volumes: 1}}}).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "placement eu-db") || !strings.Contains(out, "h3 was forgotten") {
		t.Errorf("lost row lacks its placement:\n%s", out)
	}
}

// A join command carries a token: it never lands in htmx's history cache.
func TestJoinCommandKeptOutOfHistory(t *testing.T) {
	out := render(t, joinCommand("curl one", "curl two"))
	if strings.Count(out, `hx-history="false"`) != 2 {
		t.Errorf("join command cached by history:\n%s", out)
	}
}
