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
		Envs: []RelEnv{{Name: "prod", Color: "teal", Href: "/o/immich/prod"}}, Waiting: 2, Failed: 1,
		Rows: []RelRow{
			{Kind: "building", Stack: "immich", Commit: "f9cdc50", At: at, Link: RelLink{Label: "Job log", Href: "/j/1"}},
			{Kind: "failed", Commit: "a8c41d2", Message: "Push failed: build immich-ml, exit status 1", State: "failed", Tone: "danger", Link: RelLink{Label: "Job log", Href: "/j/2"}},
			{Kind: "release", ID: "r4", Number: "4", Commit: "56abcb8", Message: "Image watch", State: "waiting", Tone: "accent", Lacks: "3", Link: RelLink{Label: "Review plan", Href: "/p/4"}, At: at, By: "image watch"},
			{Kind: "release", ID: "r3", Number: "3", Commit: "b653b0c", Message: "immich: depends_on for the server", State: "waiting", Tone: "accent", Link: RelLink{Label: "Review plan", Href: "/p/3"}, At: at, By: "git push",
				Jobs: []RelJob{{"push, done in 14s", "/j/3"}}, CommitURL: "https://github.com/a/b/commit/b653b0c"},
			{Kind: "release", ID: "r2", Number: "2", Commit: "256abcb", Message: "immich: v3.4.1", State: "running", Tone: "success", Envs: []EnvRun{{Name: "prod", Color: "teal"}}, At: at, By: "git push"},
			{Kind: "release", ID: "r1", Number: "1", Commit: "10f3e7a", Message: "first stack file", State: "superseded", At: at, By: "git push"},
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
		"Building immich at", "Push failed: build immich-ml", `data-id="r3"`, ">Review plan<", "Does not hold release 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("window lacks %q", want)
		}
	}
	if strings.Contains(body, `<details id="rel-window" class="env-compare group absolute bottom-4 left-4 z-10 open:w-[34rem] open:max-w-[calc(100%-2rem)]" open`) {
		t.Error("closed by default: ?rel=1 opens it")
	}
	b.Reset()
	w := relSample()
	w.Open = true
	_ = RelWindowView(w).Render(context.Background(), &b)
	if !strings.Contains(b.String(), `z-10 open:w-[34rem] open:max-w-[calc(100%-2rem)]" open>`) {
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
		Rows: []RelRow{
			{Kind: "plan", Scope: "org config", Commit: "7c2e9d1", Message: "1 to add: stack dashy", State: "waiting", Tone: "accent", Link: RelLink{Label: "Review plan", Href: "/o?drawer=org:1&tab=config"}},
			{Kind: "release", ID: "r15", Scope: "media 15", OfStack: true, Number: "15", Commit: "96ffc59", Message: "media: forward auth for five tools", State: "waiting", Tone: "accent", Link: RelLink{Label: "Review plan", Href: "/o/media"}, At: at, By: "git push"},
			{Kind: "release", ID: "r8", Scope: "auth 8", OfStack: true, Number: "8", Commit: "4d1a0be", Message: "auth: LLDAP and Authelia", State: "running", Tone: "success", Envs: []EnvRun{{Name: "prod", Color: "teal"}}, At: at, By: "git push"},
			{Kind: "plan", Scope: "org config", Commit: "31b88f0", Message: "2 applied: stack immich, share photos", State: "applied"},
		},
	}
	var b strings.Builder
	if err := RelWindowView(org).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	body := b.String()
	for _, want := range []string{
		`data-view="all"`, `data-view-set="org"`, `data-view-set="all"`, `data-for="all"`, `data-for="org"`, ">3 waiting<", ">1 waiting<",
		`<li data-stack>`, ">media 15<", ">org config<", ">applied<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("org window lacks %q", want)
		}
	}
	if strings.Contains(body, `<nav aria-label="Environments"`) {
		t.Error("an org window has env links")
	}
	if os.Getenv("REL_DUMP_ORG") != "" {
		w := org
		w.Open = true
		b.Reset()
		_ = RelWindowView(w).Render(context.Background(), &b)
		_ = os.WriteFile(os.Getenv("REL_DUMP_ORG"), []byte(b.String()), 0o600)
		b.Reset()
		srv := RelWindow{Title: "Server config", Waiting: 1, Failed: 0, Open: true, Rows: []RelRow{
			{Kind: "plan", Commit: "e20fe28", Message: "1 removal to review: route auth.vulpe.dev", State: "waiting", Tone: "accent", Link: RelLink{Label: "Review plan", Href: "/-/admin/config"}},
			{Kind: "plan", Commit: "b71c3e4", Message: "2 applied: routes nas, s3-console", State: "applied"},
			{Kind: "plan", Commit: "90aa1f2", Message: "Plan failed: stackr-server.yml line 14, unknown key", State: "failed", Tone: "danger", Link: RelLink{Label: "View details", Href: "/-/admin/config"}},
		}}
		_ = RelWindowView(srv).Render(context.Background(), &b)
		_ = os.WriteFile(os.Getenv("REL_DUMP_ORG")+".server", []byte(b.String()), 0o600)
	}
	b.Reset()
	if err := RelWindowView(RelWindow{Title: "Server config", Note: RelNote{"No config file is bound.", "Bind a repository", "/-/admin/config"}}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, ">Server config<") || !strings.Contains(got, "No config file is bound.") || !strings.Contains(got, ">Bind a repository<") || strings.Contains(got, "data-view") {
		t.Errorf("cluster window:\n%s", got)
	}
}
