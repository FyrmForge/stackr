package tile

import (
	"context"
	"strings"
	"testing"

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
				Vals:   map[string]string{"cpu_limit": "0.5", "port": "80"},
				Errors: map[string]string{"cpu_limit": "must be a number"},
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
				`name="fields" value="cpu_limit port"`,
				`value="0.5"`,
				"must be a number",
				"https://api.acme.dev",
				`/domains/d1/detach`,
				`name="raw_caddy"`,
				"reverse_proxy x",
				`name="host"`,
				"/image/check",
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
				Next: "Jul 25 03:00",
				Rows: []RunRow{
					{ID: "r1", Status: "running", Live: true},
				},
			}),
			[]string{
				"/run\"",
				">Pause",
				`{&#34;paused&#34;:&#34;true&#34;}`,
				"next run Jul 25 03:00",
				"/runs/r1/stop",
				"tab=logs&amp;run=r1",
			},
		},
		"backups": {
			v,
			Backups(v, BackupsView{
				Rows: []VolumeRow{{ID: "v1", Name: "uploads", Drawer: "/o/s/e/-/volumes/v1?tab=backups"}},
			}),
			[]string{"uploads", "-/volumes/v1"},
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

// v0's tab names and order per kind; the old domains and image tabs open
// Settings, a key the kind lacks opens its first tab.
func TestTabsPerKind(t *testing.T) {
	for kind, want := range map[string]string{
		"cron":    "runs logs jobs env settings backups",
		"service": "status jobs logs env settings backups",
	} {
		if got := strings.Join(Tabs(kind), " "); got != want {
			t.Errorf("%s tabs = %s", kind, got)
		}
	}
	for _, tc := range []struct{ kind, tab, want string }{
		{"cron", "status", "runs"},
		{"service", "domains", "settings"},
		{"image", "image", "settings"},
		{"service", "runs", "status"},
	} {
		if got := Tab(tc.kind, tc.tab); got != tc.want {
			t.Errorf("Tab(%s, %s) = %s, want %s", tc.kind, tc.tab, got, tc.want)
		}
	}
}
