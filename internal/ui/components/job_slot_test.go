package components

import (
	"context"
	"strings"
	"testing"
)

func TestJobPollScopes(t *testing.T) {
	for path, want := range map[string]string{
		"/-/admin/backups":                     "/-/admin/job-live?kind=a",
		"/acme/-/drawer":                       "/acme/-/job-live?kind=a",
		"/acme/web/prod/-/drawer":              "/acme/web/prod/-/job-live?kind=a",
		"/acme/web/-/drawer":                   "/acme/web/-/job-live?kind=a",
		"/acme/web/prod/-/drawer/sync/staging": "/acme/web/prod/-/job-live?kind=a",
		"/":                                    "",
	} {
		if got := jobPoll(WithPath(context.Background(), path), "a", ""); !strings.HasPrefix(got, want) || (want != "" && !strings.Contains(got, "&since=")) {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// An idle slot polls and swaps only itself; a running one holds its stream and no poll.
func TestJobSlot(t *testing.T) {
	ctx := WithPath(context.Background(), "/acme/web/prod/-/drawer")
	var b strings.Builder
	if err := JobSlot(nil, "env-sync").Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="job-slot"`, `hx-get="/acme/web/prod/-/job-live?kind=env-sync&amp;since=`, `hx-select="#job-slot"`, `every 5s`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("idle slot lacks %q: %s", want, b.String())
		}
	}
	b.Reset()
	live := &JobStatusView{Kind: "env-sync", State: "running", StreamURL: "/s", Live: true}
	if err := JobSlot(live, "env-sync").Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "every 5s") || !strings.Contains(b.String(), "sse-connect") {
		t.Errorf("running slot: %s", b.String())
	}
}
