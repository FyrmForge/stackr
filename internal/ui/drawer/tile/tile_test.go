package tile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Every tab draws from its view struct inside the drawer, with its marker.
func TestEveryTabRenders(t *testing.T) {
	v := View{
		Node:      "t1",
		Name:      "api",
		Kind:      "service",
		Source:    "github.com/acme/api",
		Base:      "/o/s/e/-/tiles/api",
		Tab:       "status",
		Error:     "nope",
		Note:      "deploy queued",
		Status:    "running",
		Location:  "shop / dev",
		NewDigest: "sha256:0123456789abcdef",
	}
	job := c.JobStatusView{
		Kind:      "deploy",
		State:     "running",
		StreamURL: "/o/s/e/-/jobs/j1/events",
		Live:      true,
	}
	cron := v
	cron.Kind = "cron"
	for tab, tc := range map[string]struct {
		v    View
		body templ.Component
		want []string
	}{
		"status": {
			v,
			Status(v, StatusView{
				Word:     "running",
				Job:      &job,
				Replicas: []ReplicaView{{Name: "api-1", State: "running"}},
				URLs:     []DomainRow{{Host: "api.acme.dev", Port: "80", HTTPS: true}},
				Port:     "80",
			}),
			[]string{
				`sse-connect="/o/s/e/-/jobs/j1/events"`,
				"api-1",
				"https://api.acme.dev",
				"Overview",
				`hx-post="/o/s/e/-/tiles/api/restart"`,
				`hx-post="/o/s/e/-/tiles/api/deploy"`,
				"New image version available",
				"0123456789ab",
				"shop / dev",
			},
		},
		"logs": {
			v,
			Logs(v, LogsView{
				Replicas: []ReplicaView{
					{ID: "c1", Name: "api-1"},
					{ID: "c2", Name: "api-2"},
				},
				Container: "c2",
				Pane:      c.LogPaneView{StreamURL: "/o/s/e/-/tiles/api/logs/stream?container=c2"},
			}),
			[]string{"<log-pane", `value="c2" selected`, "logs/stream?container=c2"},
		},
		"env": {
			v,
			Env(v, EnvView{Rows: []EnvRow{{Name: "A", Value: "1"}}, Text: "A=1"}),
			[]string{`name="env"`, "A=1", `aria-label="Delete A"`, "Variables"},
		},
		"settings": {
			v,
			Settings(v, SettingsView{
				Name:    "api",
				Kind:    "service",
				Vals:    map[string]string{"cpu_limit": "0.5", "mem_limit_mb": "", "port": "80", "image": "nginx:1", "volumes": ""},
				Errors:  map[string]string{"cpu_limit": "must be a number"},
				From:    map[string]string{"mem_limit_mb": "org"},
				Inherit: map[string]string{"mem_limit_mb": "512"},
				Open:    "resources",
				Volumes: []VolumeRow{{ID: "v1", Name: "uploads", Drawer: "/o/s/e/-/volumes/v1?tab=backups"}},
				Placement: "/o/s/e/-/tiles/api/placement",
				Domains: &DomainsView{
					Admin: true,
					Rows: []DomainRow{{
						ID:    "d1",
						Host:  "api.acme.dev",
						Port:  "80",
						HTTPS: true,
						Raw:   "reverse_proxy x",
					}},
				},
				Image: &ImageView{Ref: "nginx:1", Digest: "sha256:a", LastDigest: "sha256:b"},
			}),
			[]string{
				`id="tile-settings"`,
				`<settings-find`,
				`name="fields" value="cpu_limit mem_limit_mb"`,
				`name="fields" value="port"`,
				`hx-post="/o/s/e/-/tiles/api/settings?open=resources"`,
				`data-acc="resources" data-changed open`,
				`value="0.5"`,
				`placeholder="512"`,
				"from org",
				"0.5 cores · 512 MB (from org)",
				"80 http · 1 domain",
				"must be a number",
				"https://api.acme.dev",
				`/domains/d1/detach?open=network`,
				`name="raw_caddy"`,
				"reverse_proxy x",
				`name="host"`,
				"/image/check?open=source",
				`hx-get="/o/s/e/-/tiles/api/placement"`,
				"uploads",
				"-/volumes/v1",
				`/rename?open=general`,
				`word="api"`,
			},
		},
		"jobs": {
			v,
			Jobs(v, JobsView{
				Rows: []JobRow{
					{Kind: "deploy", State: "failed", Error: "health check timed out"},
				},
			}),
			[]string{"health check timed out", "Deployments"},
		},
		"runs": {
			cron,
			Runs(cron, RunsView{
				Next: time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC),
				Rows: []RunRow{
					{ID: "r1", Status: "running", Live: true},
				},
			}),
			[]string{
				"/run\"",
				">Pause",
				`{&#34;paused&#34;:&#34;true&#34;}`,
				"next run <local-time fmt=\"Jan 2 15:04\"><time datetime=\"2026-07-25T03:00:00Z\"",
				"Times in <local-time zone>UTC</local-time>",
				"/runs/r1/stop",
				"tab=logs&amp;run=r1",
			},
		},
		"access": {
			v,
			Access(v, AccessView{
				Entries: []AccessEntry{
					{
						Slice:  "main",
						Link:   "/o/s/e?drawer=t5&tab=overview",
						Access: "read",
					},
				},
				Bindings: []AccessBinding{
					{
						Slice:  "cache",
						Link:   "/o/s/e?drawer=t6&tab=overview",
						Access: "write",
						User:   "api_user",
					},
				},
				Slices: []string{"cache", "main"},
			}),
			[]string{
				`href="/o/s/e?drawer=t5&amp;tab=overview"`,
				`name="slice" value="main"`,
				`<option value="read" selected>`,
				`name="access" value="default"`,
				`aria-label="Use the default access on main"`,
				`href="/o/s/e?drawer=t6&amp;tab=overview"`,
				"api_user",
				`id="access-slice"`,
				`hx-post="/o/s/e/-/tiles/api/access?open=dbaccess"`,
			},
		},
	} {
		var b strings.Builder
		if err := Drawer(tc.v, tc.body).Render(context.Background(), &b); err != nil {
			t.Fatalf("%s: %v", tab, err)
		}
		out := b.String()
		for _, w := range append(
			tc.want,
			`id="drawer-view"`,
			"nope",
			"deploy queued",
			`hx-push-url="?drawer=t1&amp;tab=settings"`,
		) {
			if !strings.Contains(out, w) {
				t.Errorf("%s tab lacks %q:\n%s", tab, w, out)
			}
		}
	}
}

// The tabs per kind; a key the kind lacks (the folded env, access,
// placement and backups among them) opens its first tab.
func TestTabsPerKind(t *testing.T) {
	for kind, want := range map[string]string{
		"cron":    "runs logs metrics jobs settings",
		"service": "status jobs logs metrics settings",
	} {
		if got := strings.Join(Tabs(kind), " "); got != want {
			t.Errorf("%s tabs = %s", kind, got)
		}
	}
	if got := strings.Join(View{Kind: "image", Terminal: true}.tabs(), " "); got != "status jobs logs terminal metrics settings" {
		t.Errorf("image tabs with a shell = %s", got)
	}
	for _, tc := range []struct{ kind, tab, want string }{
		{"cron", "status", "runs"},
		{"service", "env", "status"},
		{"cron", "placement", "runs"},
		{"service", "runs", "status"},
		{"service", "terminal", "terminal"},
		{"cron", "terminal", "runs"},
	} {
		if got := Tab(tc.kind, tc.tab, true, false); got != tc.want {
			t.Errorf("Tab(%s, %s) = %s, want %s", tc.kind, tc.tab, got, tc.want)
		}
	}
	if got := Tab("service", "settings", false, true); got != "settings" { // read-only, but there
		t.Errorf("a viewer's settings tab = %s", got)
	}
}

// Each closed row sums up its values in one line.
func TestSummaries(t *testing.T) {
	img := SettingsView{Name: "web", Kind: "image", Vals: map[string]string{
		"image": "nginx:1.27", "update_policy": "manual", "command": "", "restart_policy": "", "replicas": "",
		"port": "8080", "endpoint_protocol": "", "volumes": "data:/data\n", "files": "",
		"health_path": "/healthz", "healthcheck_cmd": "", "healthcheck_interval_s": "30",
		"cpu_limit": "", "mem_limit_mb": "512", "privileged": "", "published_ports": "80:8080\n443:8443",
	}, From: map[string]string{"cpu_limit": "org"}, Inherit: map[string]string{"cpu_limit": "1"},
		Domains: &DomainsView{Rows: make([]DomainRow, 2)}, Ingress: "h4 (from env)",
		Env: EnvView{Rows: make([]EnvRow, 8)}, Access: AccessView{Entries: []AccessEntry{{Slice: "main", Access: "read"}}}}
	repo := SettingsView{Kind: "function", Vals: map[string]string{
		"git_url": "https://github.com/acme/api", "branch": "", "trigger": "on_deploy", "timeout_minutes": "30",
		"dockerfile": "", "build_context": "web", "build_args": "A=1\nB=2", "command": "make run",
	}}
	for _, tc := range []struct{ name, got, want string }{
		{"general", generalSummary(img), "web · image"},
		{"source image", sourceSummary(img), "nginx:1.27 · notify on a new version"},
		{"source repo", sourceSummary(repo), "github.com/acme/api · default branch"},
		{"build", buildSummary(repo), "Dockerfile · context web · 2 build args"},
		{"schedule", scheduleSummary(repo), "runs on every deploy · max 30 min"},
		{"run image", runSummary(img), "image command · restart always · 1 replica"},
		{"run repo", runSummary(repo), "own command"},
		{"network", networkSummary(img), "8080 http · 2 domains · ingress h4 (from env)"},
		{"storage", storageSummary(img), "1 volume · no files"},
		{"health", healthSummary(img), "GET /healthz · every 30 s"},
		{"health none", healthSummary(repo), "no check"},
		{"resources", resourcesSummary(img), "1 core (from org) · 512 MB"},
		{"variables", variablesSummary(img), "8 set"},
		{"access", accessSummary(img), "main read"},
		{"access none", accessSummary(repo), "none"},
		{"host", hostSummary(img), "2 server ports"},
		{"host none", hostSummary(repo), "none, needs an admin"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// A field is changed when it differs from what the tile holds unset; a
// row is marked when one of its fields is, and only rows the kind has
// draw.
func TestSettingsRowsChanged(t *testing.T) {
	s := SettingsView{Kind: "image", Vals: map[string]string{
		"image": "nginx:1", "update_policy": "manual", "replicas": "1", "restart_policy": "always",
		"port": "8080", "endpoint_protocol": "http", "cpu_limit": "", "mem_limit_mb": "",
	}}
	got := map[string]bool{}
	var keys []string
	for _, r := range settingsRows(View{}, s) {
		got[r.Key] = r.Changed
		keys = append(keys, r.Key)
	}
	if strings.Join(keys, " ") != "general source run network resources variables dbaccess delete" {
		t.Errorf("rows = %v", keys)
	}
	if got["source"] || got["run"] || got["resources"] || !got["network"] {
		t.Errorf("changed rows = %v", got)
	}
}

// Restart is safe to repeat, so it posts without a dialog; Stop still asks.
func TestRestartHasNoConfirm(t *testing.T) {
	var b strings.Builder
	v := View{Name: "api", Kind: "service", Base: "/t"}
	if err := actions(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); strings.Contains(got, "Restart api?") || !strings.Contains(got, `hx-post="/t/restart"`) ||
		!strings.Contains(got, "Stop api?") {
		t.Errorf("header actions:\n%s", got)
	}
}

// A generated redirect row is a 308 in Caddy, a declared one a 301.
func TestDomainRedirectCode(t *testing.T) {
	var b strings.Builder
	d := DomainsView{Rows: []DomainRow{
		{ID: "a", Host: "old.acme.dev", Redirect: "new.acme.dev", Auto: true},
		{ID: "b", Host: "x.acme.dev", Redirect: "y.acme.dev"},
	}}
	if err := domains(View{Base: "/t"}, d).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.Contains(got, "308 → new.acme.dev") || !strings.Contains(got, "301 → y.acme.dev") {
		t.Errorf("redirect codes:\n%s", got)
	}
}

// A skipped run is a stop, faint like a cancelled one, not a red failure.
func TestSkippedRunIsFaint(t *testing.T) {
	if got := runTone(RunRow{Status: "skipped"}); got != "text-rw-faint" {
		t.Errorf("tone = %q", got)
	}
}

// Requests and Response time need a domain to ever have data.
func TestMetricsRequestChartsNeedDomain(t *testing.T) {
	m := c.MetricsView{Range: "1h", Pick: func(string) any { return map[string]any{} }}
	for _, domained := range []bool{false, true} {
		var sb strings.Builder
		if err := Metrics(View{Domained: domained}, m).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(sb.String(), "Response time"); got != domained {
			t.Errorf("domained %v: Response time chart = %v", domained, got)
		}
		if !strings.Contains(sb.String(), "CPU") {
			t.Errorf("domained %v: CPU chart missing", domained)
		}
	}
}

// A cron or function tile graphs its runs, not replicas or requests.
func TestMetricsRunKinds(t *testing.T) {
	m := c.MetricsView{Range: "1h", Pick: func(string) any { return map[string]any{} }}
	for _, kind := range []string{"cron", "function"} {
		var sb strings.Builder
		if err := Metrics(View{Kind: kind, Domained: true}, m).Render(context.Background(), &sb); err != nil {
			t.Fatal(err)
		}
		got := sb.String()
		for _, want := range []string{"Run duration", `measures="run_s"`, "Peak memory", `measures="run_mem_peak"`, "CPU", `measures="run_cpu_s"`, "Failure rate", `measures="run_failed"`, `labels="duration"`, `labels="failed"`} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: metrics tab lacks %q", kind, want)
			}
		}
		for _, not := range []string{"Memory<", "Network", "Disk IO", "Requests", "cpu_pct"} {
			if strings.Contains(got, not) {
				t.Errorf("%s: metrics tab has %q", kind, not)
			}
		}
	}
}
