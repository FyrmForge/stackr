package components

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPushFailedNoDoubleStop(t *testing.T) {
	var b strings.Builder
	if err := PushFailed(NewPushFail("remote: denied.", "/x")).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); strings.Contains(got, "..") || !strings.Contains(got, "denied.<") {
		t.Errorf("line = %s", got)
	}
}

func TestLiveLines(t *testing.T) {
	var b strings.Builder
	at := time.Date(2026, 11, 19, 10, 15, 0, 0, time.UTC)
	ls := []LiveLine{NewBuilding("huginn", "f9cdc50", at, false, "/o/s/e/-/jobs/j1"), NewParkedOn("huginn.oidc_secret", "/o/s/e?drawer=env%3A1&tab=variables")}
	if err := LiveLines(ls).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"Building huginn at f9cdc50, started ", ">10:15<", `href="/o/s/e/-/jobs/j1"`, "Job log", "Waiting for huginn.oidc_secret", "Set it"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if q := NewBuilding("huginn", "f9cdc50", at, true, ""); !strings.HasPrefix(q.Text, "Waiting to build huginn at f9cdc50, queued") {
		t.Errorf("queued = %q", q.Text)
	}
}
