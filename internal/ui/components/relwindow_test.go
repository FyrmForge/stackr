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
			{Kind: "building", Stack: "immich", Commit: "f9cdc50", At: at, Link: RelLink{"Job log", "/j/1"}},
			{Kind: "failed", Commit: "a8c41d2", Message: "Push failed: build immich-ml, exit status 1", State: "failed", Tone: "danger", Link: RelLink{"Job log", "/j/2"}},
			{Kind: "release", ID: "r4", Number: "4", Commit: "56abcb8", Message: "Image watch", State: "waiting", Tone: "accent", Lacks: "3", Link: RelLink{"Review plan", "/p/4"}, At: at, By: "image watch"},
			{Kind: "release", ID: "r3", Number: "3", Commit: "b653b0c", Message: "immich: depends_on for the server", State: "waiting", Tone: "accent", Link: RelLink{"Review plan", "/p/3"}, At: at, By: "git push",
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
