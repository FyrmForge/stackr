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
	v := View{Node: "t1", Name: "api", Kind: "cron", Base: "/o/s/e/-/tiles/api", Tab: "status", Error: "nope", Note: "deploy queued"}
	job := c.JobStatusView{Kind: "deploy", State: "running", StreamURL: "/o/s/e/-/jobs/j1/events", Live: true}
	for tab, tc := range map[string]struct {
		body templ.Component
		want []string
	}{
		"status": {Status(v, StatusView{Word: "running", Job: &job, Replicas: []ReplicaView{{Name: "api-1", State: "running"}}, NextRun: "Jul 25 03:00"}),
			[]string{`sse-connect="/o/s/e/-/jobs/j1/events"`, "api-1", "next Jul 25 03:00", `hx-post="/o/s/e/-/tiles/api/restart"`, `word="api"`}},
		"logs": {Logs(v, LogsView{Replicas: []ReplicaView{{ID: "c1", Name: "api-1"}, {ID: "c2", Name: "api-2"}}, Container: "c2",
			Pane: c.LogPaneView{StreamURL: "/o/s/e/-/tiles/api/logs/stream?container=c2"}}),
			[]string{"<log-pane", `value="c2" selected`, "logs/stream?container=c2"}},
		"domains": {Domains(v, DomainsView{Admin: true, Rows: []DomainRow{{ID: "d1", Host: "api.acme.dev", Port: "80", HTTPS: true, Raw: "reverse_proxy x"}}}),
			[]string{"https://api.acme.dev", `/domains/d1/detach`, `name="raw_caddy"`, "reverse_proxy x", `name="host"`}},
		"env":      {Env(v, EnvView{JSON: `{"A":"1"}`}), []string{`name="env_json"`, `{&#34;A&#34;:&#34;1&#34;}`}},
		"settings": {Settings(v, c.SettingsFormView{ID: "tile-settings", Action: "/x", Scope: "Tile", Rows: []c.SettingRowView{{Key: "cpu_limit", Type: "float"}}}), []string{`id="tile-settings"`, "cpu_limit"}},
		"jobs":     {Jobs(v, JobsView{Rows: []JobRow{{Kind: "deploy", State: "failed", Error: "health check timed out"}}}), []string{"health check timed out"}},
		"image":    {Image(v, ImageView{Ref: "nginx:1", Digest: "sha256:a", LastDigest: "sha256:b", NewVersion: true, Pulls: true}), []string{"A new version is out.", "/image/check", `name="update_policy"`, `name="image_ref"`}},
		"runs": {Runs(v, RunsView{Cron: true, Rows: []RunRow{{ID: "r1", Status: "running", Live: true}}}),
			[]string{"/run\"", "Pause schedule", `{&#34;paused&#34;:&#34;true&#34;}`, "/runs/r1/stop", "tab=logs&amp;run=r1"}},
		"backups": {Backups(v, BackupsView{Rows: []VolumeRow{{ID: "v1", Name: "uploads", Drawer: "/o/s/e/-/volumes/v1?tab=backups"}}}), []string{"uploads", "-/volumes/v1"}},
	} {
		var b strings.Builder
		if err := Drawer(v, tc.body).Render(context.Background(), &b); err != nil {
			t.Fatalf("%s: %v", tab, err)
		}
		out := b.String()
		for _, w := range append(tc.want, `id="drawer-view"`, "nope", "deploy queued", `hx-push-url="?drawer=t1&amp;tab=runs"`) {
			if !strings.Contains(out, w) {
				t.Errorf("%s tab lacks %q:\n%s", tab, w, out)
			}
		}
	}
}

func TestTabsPerKind(t *testing.T) {
	if got := strings.Join(Tabs("cron"), " "); !strings.Contains(got, "runs") || strings.Contains(got, "domains") {
		t.Errorf("cron tabs = %s", got)
	}
	if got := strings.Join(Tabs("service"), " "); strings.Contains(got, "runs") || strings.Contains(got, "image") {
		t.Errorf("service tabs = %s", got)
	}
}
