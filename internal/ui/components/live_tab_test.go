package components

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderLive(t *testing.T, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestLiveTabPollsAndGuards(t *testing.T) {
	got := renderLive(t, LiveTab("/x?tab=a", "tab-a"))
	for _, want := range []string{`hx-get="/x?tab=a"`, `every 3s`, `hx-select="#tab-a"`, `hx-target="#tab-a"`, `outerHTML`, `data-poll-guard=`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestBannersPoll(t *testing.T) {
	got := renderLive(t, Banners(Shell{BannersURL: "/-/banners", Upgrading: true}))
	for _, want := range []string{`id="banners"`, `hx-get="/-/banners"`, `id="upgrading"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(renderLive(t, Banners(Shell{})), "hx-get") {
		t.Error("a shell with no BannersURL polls")
	}
}

func TestJobStatusIdlePollsRefresh(t *testing.T) {
	got := renderLive(t, JobStatus(JobStatusView{Kind: "deploy", State: "done", Refresh: "/d?tab=t"}))
	if !strings.Contains(got, `hx-get="/d?tab=t"`) || !strings.Contains(got, "every 5s") {
		t.Errorf("an idle job does not poll its refresh: %s", got)
	}
	if strings.Contains(renderLive(t, JobStatus(JobStatusView{Kind: "deploy", State: "done"})), "hx-get") {
		t.Error("no Refresh must not poll")
	}
}

func TestReleasesLive(t *testing.T) {
	if !strings.Contains(renderLive(t, Releases(ReleasesView{Live: "/d?tab=releases"})), `hx-select="#tab-releases"`) {
		t.Error("releases not live")
	}
	if strings.Contains(renderLive(t, Releases(ReleasesView{Live: "/d", Ask: &ConfirmView{}})), "hx-get") {
		t.Error("releases polls over an open question")
	}
}
