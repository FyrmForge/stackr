package tile

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// Lists that change under the viewer poll themselves: jobs and runs every
// 2s while one moves and every 5s when idle; the access, domains and image
// regions swap just themselves; those three do not poll in a view drawn
// without a stream.
func TestLivePolls(t *testing.T) {
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Base: "/o/s/e/-/tiles/api", Kind: "cron", Live: "/o/s/e/-/tiles/api/live"}
	for name, tc := range map[string]struct {
		html string
		want []string
	}{
		"jobs idle":  {render(Jobs(v, JobsView{Poll: "/jobs"})), []string{`hx-get="/jobs"`, `every 5s`}},
		"jobs fast":  {render(Jobs(v, JobsView{Poll: "/jobs", Fast: true})), []string{`every 2s`}},
		"runs idle":  {render(Runs(v, RunsView{Poll: "/runs"})), []string{`hx-get="/runs"`, `every 5s`}},
		"runs fast":  {render(Runs(v, RunsView{Poll: "/runs", Fast: true})), []string{`every 2s`}},
		"logs empty": {render(Logs(v, LogsView{})), []string{`every 3s`, `/tiles/api?tab=logs`}},
		"domains":    {render(domainList(v, DomainsView{})), []string{`id="live-domains"`, `hx-select="#live-domains"`, `data-poll-guard="region"`}},
		"image":      {render(image(v, ImageView{})), []string{`id="live-image"`, `hx-select="#live-image"`}},
		"access":     {render(Access(v, AccessView{})), []string{`id="live-bindings"`, `hx-select="#live-bindings"`}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.html, w) {
				t.Errorf("%s lacks %q:\n%s", name, w, tc.html)
			}
		}
	}
	v.Live = ""
	for name, html := range map[string]string{
		"domains": render(domainList(v, DomainsView{})), "image": render(image(v, ImageView{})),
		"bindings": render(Access(v, AccessView{})),
	} {
		if strings.Contains(html, "hx-trigger") {
			t.Errorf("%s polls without a stream", name)
		}
	}
}

// The Status tab's regions are the same markup in the page and in the
// stream, and the header buttons ride their own stream.
func TestStatusRegions(t *testing.T) {
	render := func(c templ.Component) string {
		var b strings.Builder
		if err := c.Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	v := View{Base: "/o/s/e/-/tiles/api", Kind: "service", Tab: "status", Live: "/o/s/e/-/tiles/api/live"}
	s := StatusView{Word: "running", Replicas: []ReplicaView{{Name: "api-1", State: "running"}}}
	page := render(Status(v, s))
	for _, region := range []string{"status", "lastjob", "urls", "access"} {
		if !strings.Contains(page, `sse-swap="`+region+`"`) {
			t.Errorf("no %s region", region)
		}
	}
	if m := render(LiveStatus(v, s)); !strings.Contains(page, m) || !strings.Contains(m, "api-1") {
		t.Errorf("the status region differs between the page and the stream: %q", m)
	}
	head := render(actions(v))
	if !strings.Contains(head, `sse-connect="/o/s/e/-/tiles/api/live?part=head"`) || !strings.Contains(head, `sse-swap="actions"`) || !strings.Contains(head, "Restart") {
		t.Errorf("header actions are not live:\n%s", head)
	}
	if v.Tab = "terminal"; strings.Contains(render(actions(v)), "sse-connect") {
		t.Error("the Terminal tab opens a header stream")
	}
}
