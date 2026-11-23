package components

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func relSample() RelWindow {
	at := time.Date(2026, 11, 18, 20, 51, 0, 0, time.UTC)
	return RelWindow{
		Envs: []RelEnv{{Name: "prod", Color: "teal", Href: "/o/immich/prod"}}, Waiting: 2, Failed: 1, All: "/-/releases?scope=o%2Fimmich",
		Rows: []ReleaseLineView{
			{Stack: "immich", Cause: "Building at f9cdc50", At: at, Link: RelLink{Label: "Job log", Href: "/j/1"}},
			{Stack: "immich", Commit: "a8c41d2", Cause: "Push failed: build immich-ml, exit status 1", State: "failed", Tone: "danger", Link: RelLink{Label: "Job log", Href: "/j/2"}},
			{Stack: "immich", Number: "4", Change: "immich-server 3.4.1 to 3.4.2", State: "waiting", Tone: "accent", Lacks: "3", Open: "/o/immich/prod/-/releases/r4/dialog", At: at, Source: "image watch", By: "image watch", To: "prod"},
			{Stack: "immich", Number: "3", Commit: "b653b0c", Message: "immich: depends_on for the server", State: "waiting", Tone: "accent", Open: "/o/immich/prod/-/releases/r3/dialog", At: at, Source: "git push", By: "git push"},
			{Stack: "immich", Number: "2", Commit: "256abcb", Message: "immich: v3.4.1", State: "running", Tone: "success", Runs: []EnvRun{{Name: "prod", Color: "teal"}}, At: at, Source: "git push", By: "git push"},
			{Stack: "immich", Number: "1", Commit: "10f3e7a", Message: "first stack file", State: "superseded", At: at, Source: "git push", By: "git push"},
		},
	}
}

// The bar says waiting and failed as words with a count, the rows lead with
// the commit, and a release row opens in place.
func TestRelWindow(t *testing.T) {
	var b strings.Builder
	if err := RelWindowView(relSample()).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	for _, want := range []string{
		">Releases<", ">2 waiting<", ">1 failed<", `hx-get="/o/immich/prod"`,
		"Building at f9cdc50", "Push failed: build immich-ml", `hx-get="/o/immich/prod/-/releases/r3/dialog"`, `hx-target="#rel-modal"`, `id="rel-modal"`,
		"Does not hold release 3", "immich-server 3.4.1 to 3.4.2", ">All releases<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("window lacks %q", want)
		}
	}
	for _, gone := range []string{"Updates by itself", "Review plan", "Review rollback"} {
		if strings.Contains(body, gone) {
			t.Errorf("a window row carries %q", gone)
		}
	}
	if strings.Contains(body, `<details id="rel-window" class="env-compare group absolute bottom-4 left-4 z-10 open:w-[38rem] open:max-w-[calc(100%-2rem)]" open`) {
		t.Error("closed by default: ?rel=1 opens it")
	}
	b.Reset()
	w := relSample()
	w.Open = true
	_ = RelWindowView(w).Render(context.Background(), &b)
	if !strings.Contains(b.String(), `z-10 open:w-[38rem] open:max-w-[calc(100%-2rem)]" open>`) {
		t.Error("?rel=1 did not open it")
	}
	if os.Getenv("REL_DUMP") != "" { // a look at the sketch: REL_DUMP=file go test -run TestRelWindow
		_ = os.WriteFile(os.Getenv("REL_DUMP"), []byte(b.String()), 0o600)
	}
	b.Reset()
	_ = RelWindowView(RelWindow{Err: true}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "Releases could not be loaded.") {
		t.Error("no cannot-load line")
	}
	b.Reset()
	_ = RelWindowView(RelWindow{}).Render(context.Background(), &b)
	if !strings.Contains(b.String(), "No releases yet. A push or a first deploy makes one.") {
		t.Error("no empty line")
	}
}

// The org chart's window: the switch, a set of counts per view, rows that
// start with where they belong, a stack's rows marked for the style sheet to
// leave out of "Org config"; the cluster chart's is titled and has no env
// links.
func TestRelWindowOrgAndCluster(t *testing.T) {
	at := time.Date(2026, 11, 18, 20, 51, 0, 0, time.UTC)
	org := RelWindow{
		Switch: true, View: "all", Waiting: 3, OrgWaiting: 1,
		Rows: []ReleaseLineView{
			{Stack: "org config", Commit: "7c2e9d1", Cause: "1 to add: stack dashy", State: "waiting", Tone: "accent", Link: RelLink{Label: "Review plan", Href: "/o?drawer=org:1&tab=config"}},
			{Stack: "media", Number: "15", OfStack: true, Commit: "96ffc59", Message: "media: forward auth for five tools", State: "waiting", Tone: "accent", Open: "/o/media/prod/-/releases/r15/dialog", At: at, Source: "git push", By: "git push"},
			{Stack: "auth", Number: "8", OfStack: true, Commit: "4d1a0be", Message: "auth: LLDAP and Authelia", State: "running", Tone: "success", Runs: []EnvRun{{Name: "prod", Color: "teal"}}, At: at, Source: "git push", By: "git push"},
			{Stack: "org config", Commit: "31b88f0", Cause: "2 applied: stack immich, share photos", State: "applied"},
		},
	}
	var b strings.Builder
	if err := RelWindowView(org).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	for _, want := range []string{
		`data-view="all"`, `data-view-set="org"`, `data-view-set="all"`, `data-for="all"`, `data-for="org"`, ">3 waiting<", ">1 waiting<",
		`<li data-stack>`, ">media<", ">release 15<", ">org config<", ">applied<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("org window lacks %q", want)
		}
	}
	if strings.Contains(body, `<nav aria-label="Environments"`) {
		t.Error("an org window has env links")
	}
	b.Reset()
	if err := RelWindowView(RelWindow{Title: "Server config", Note: RelNote{"No config file is bound.", "Bind a repository", "/-/admin/config"}}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, ">Server config<") || !strings.Contains(got, "No config file is bound.") || !strings.Contains(got, ">Bind a repository<") || strings.Contains(got, "data-view") {
		t.Errorf("cluster window:\n%s", got)
	}
}

// A failed read keeps the bar's last counts, marked with their time, and the
// list says it could not load with a way to try again; the first read that
// fails has no counts to keep.
func TestRelKeeperKeepsCountsOnError(t *testing.T) {
	var k RelKeeper
	at := time.Date(2026, 11, 19, 21, 4, 0, 0, time.UTC)
	if got := k.Hold(&RelWindow{Err: true, Retry: "/x?rel=1"}, at); got.Waiting != 0 || got.AsOf != "" || !got.Err {
		t.Errorf("first read failing = %+v", got)
	}
	k.Hold(&RelWindow{Waiting: 2, Failed: 1, Retry: "/x?rel=1"}, at)
	w := k.Hold(&RelWindow{Err: true, Retry: "/x?rel=1"}, at.Add(time.Minute))
	var b strings.Builder
	_ = RelUpdate(*w).Render(context.Background(), &b)
	for _, want := range []string{"2 waiting", "1 failed", "as of 21:04", "Releases could not be loaded.", `href="/x?rel=1"`, "Try again"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("update lacks %q:\n%s", want, b.String())
		}
	}
	if got := k.Hold(&RelWindow{Waiting: 1}, at.Add(2*time.Minute)); got.AsOf != "" || got.Err {
		t.Errorf("a good read after = %+v", got)
	}
}
