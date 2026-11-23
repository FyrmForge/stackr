package components

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A clean waiting release offers Promote; a risky one lists its problems and
// points at the full review only.
func TestReleaseModal(t *testing.T) {
	var b strings.Builder
	v := ReleaseModalView{Stack: "immich", Number: "4", Env: "prod", Source: "git push", By: "push", Commit: "abc1234", Who: "ann", Via: "GitHub", Age: "3d ago", At: time.Date(2026, 11, 20, 7, 20, 0, 0, time.UTC), Review: "/o/s/prod/-/releases/r4",
		Changes: []ReleaseChangeView{{Label: "api port", Old: "80", New: "8080"}}, Promote: "/-/waiting/promote", Pair: "r4:e1"}
	if err := ReleaseModal(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{">immich<", "Release 4", "modal-grab", `aria-labelledby="rel-modal-title rel-modal-num"`, "api port", `hx-post="/-/waiting/promote"`, `r4:e1`, `hx-swap="none"`, "Open full review", `id="rel-modal"`, "3d ago via GitHub", `title="Nov 20 2026, 07:20 UTC"`, ">ann<"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("modal lacks %q", want)
		}
	}
	b.Reset()
	v.Promote, v.Problems, v.Gaps = "", []string{"db.pass not set"}, []string{"db.pass not set"}
	_ = ReleaseModal(v).Render(context.Background(), &b)
	if strings.Contains(b.String(), ">Promote<") || !strings.Contains(b.String(), "Needs full review") || !strings.Contains(b.String(), "db.pass not set") {
		t.Error("risky modal must list problems and offer no Promote")
	}
}

// The sections come in order, each only when filled, and a param shows only
// "changed".
func TestReleaseModalSections(t *testing.T) {
	var b strings.Builder
	v := ReleaseModalView{Stack: "api", Number: "7", Env: "prod", Review: "/r", State: "waiting", Tone: "accent",
		Problems: []string{"x"}, Blockers: []string{"needs an owner"}, Impacts: []string{"api restarts"}, Removals: []string{"route x"}, Gaps: []string{"db.pass not set"},
		Changes: []ReleaseChangeView{{Label: "api image", Old: "1.0", New: "1.1"}, {Label: "param db.pass", New: "changed"}}}
	if err := ReleaseModal(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	out, at := b.String(), 0
	for _, w := range []string{">waiting<", "Blocks the promote", "needs an owner", ">Impact<", "api restarts", ">Removes<", "Values to set", "db.pass not set", "What changed in prod", "line-through", ">1.1<", ">changed<"} {
		i := strings.Index(out[at:], w)
		if i < 0 {
			t.Fatalf("missing or out of order: %q", w)
		}
		at += i
	}
	b.Reset()
	_ = ReleaseModal(ReleaseModalView{Env: "prod", Review: "/r", Impacts: []string{"i"}, Problems: []string{"i"}}).Render(context.Background(), &b)
	if strings.Contains(b.String(), "Blocks the promote") || strings.Contains(b.String(), "Values to set") {
		t.Error("an empty section must not render")
	}
}

func TestReleaseLine(t *testing.T) {
	var b strings.Builder
	v := ReleaseLineView{Stack: "immich", Number: "4", Change: "immich-server 3.4.1 to 3.4.2", Source: "image watch", By: "image-watch", To: "prod", Waited: "waiting 3d", State: "waiting", Tone: "accent"}
	if err := ReleaseLine(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"immich", "release 4", "3.4.1 to 3.4.2", "waiting 3d", "-&gt; prod"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("line lacks %q", want)
		}
	}
	if strings.Contains(b.String(), "font-mono\">") {
		t.Error("only a push shows a commit")
	}
	if strings.Contains(b.String(), "could not be read") {
		t.Error("a read label has no note")
	}
	b.Reset()
	v.NoVersion = true
	_ = ReleaseLine(v).Render(context.Background(), &b)
	if !strings.Contains(b.String(), `title="version label could not be read"`) {
		t.Error("an unread label lacks its note")
	}
}

// A row with an Open address is a button that fills the modal slot; a long
// name, the release number and the change never truncate, the commit message
// does.
func TestReleaseLineOpensAndTruncates(t *testing.T) {
	var b strings.Builder
	v := ReleaseLineView{Stack: "a-very-long-stack-name", Number: "4", Cause: "Rebuild of release 3", Commit: "abc1234", Message: "fix the thing", Open: "/o/s/prod/-/releases/r4/dialog"}
	if err := ReleaseLine(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`<button type="button" hx-get="/o/s/prod/-/releases/r4/dialog"`, `hx-target="#rel-modal"`, `hx-swap="outerHTML"`, `class="w-full min-w-0 truncate" title="fix the thing"`} {
		if !strings.Contains(out, want) {
			t.Errorf("row lacks %q", want)
		}
	}
	if strings.Contains(out, "truncate\">a-very-long") || strings.Contains(out, "truncate\">Rebuild") || strings.Contains(out, "truncate\">release") {
		t.Error("name, release number and cause must not truncate")
	}
	b.Reset()
	v.Open = ""
	_ = ReleaseLine(v).Render(context.Background(), &b)
	if strings.Contains(b.String(), "hx-get") {
		t.Error("a row with no Open is not clickable")
	}
}

// The Waiting page: ticks only on rows that can be promoted, one bulk,
// "N more" on a risky row, the phone link to the full review.
func TestWaitingReleaseRows(t *testing.T) {
	clean := &WaitReleaseView{Line: ReleaseLineView{Stack: "immich", Number: "4"}, Pair: "r4:e1", Promote: true}
	risky := &WaitReleaseView{Line: ReleaseLineView{Stack: "bazarr", Number: "2", Open: "/o/s/prod/-/releases/r2/dialog?on=waiting"}, Pair: "r2:e1", Promote: true,
		Problems: []string{"Value db.pass is not set", "Removes route x", "api restarts"}, Review: "/o/s/prod/-/releases/r2"}
	v := WaitingView{Count: 2, Promote: "/-/waiting/promote", Notice: "1 of 3 skipped: needs full review", Groups: []WaitGroupView{{Title: "Releases to promote", Rows: []WaitRowView{
		{Title: "t", Rel: clean, Href: "/x", Act: "Review plan"}, {Title: "t", Rel: risky, Href: "/y", Act: "Review plan"},
		{Title: "t", Rel: &WaitReleaseView{Line: ReleaseLineView{Stack: "lidarr", Number: "7"}, Pair: "r7:e1", PlanFailed: true}, Href: "/z", Act: "Review plan"}}}}}
	var b strings.Builder
	if err := Waiting(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"<waiting-select", "data-all", "data-bulk", `hx-post="/-/waiting/promote"`, "Needs full review", "Value db.pass is not set, and 2 more",
		`href="/o/s/prod/-/releases/r2"`, "1 of 3 skipped: needs full review", `id="rel-modal"`} {
		if !strings.Contains(out, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(out, ">Review plan<") != 3 || !strings.Contains(out, "Plan could not be read") {
		t.Error("every row keeps Review plan; a failed plan says so")
	}
	if n := strings.Count(out, `value="r4:e1"`); n != 1 || strings.Count(out, "disabled") < 1 {
		t.Errorf("ticks: %d for the clean row", n)
	}
	if strings.Count(out, ">Promote<") != 3 { // the clean row, the bulk opener and its yes
		t.Errorf("Promote buttons = %d, want 3", strings.Count(out, ">Promote<"))
	}
}

// The avatar keeps initials underneath and layers the picture on top, with no
// referrer and no alt; a bot is a rounded square.
func TestAvatarMark(t *testing.T) {
	var b strings.Builder
	_ = AvatarMark(Avatar{Initials: "AL", Login: "ada", Badge: "github", Bot: true}).Render(context.Background(), &b)
	for _, want := range []string{">AL", `<img src="https://avatars.githubusercontent.com/ada?s=64" alt="" referrerpolicy="no-referrer"`, "rounded-md"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("avatar lacks %q: %s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "onerror") {
		t.Error("inline onerror is blocked by CSP")
	}
}

// Line 2 reads "sha · author · 3d ago via GitHub", then Waiting's words.
func TestReleaseLineSecondLine(t *testing.T) {
	out := renderLive(t, ReleaseLine(ReleaseLineView{Stack: "s", Number: "3", Commit: "abc1234", Message: "fix header", Who: "Ada", Via: "GitHub", Age: "3d ago",
		At: time.Date(2026, 11, 20, 7, 20, 0, 0, time.UTC), Waited: "waiting 3d", From: "staging", To: "prod"}))
	want := []string{`font-mono" title="">abc1234`, ">Ada<", `title="Nov 20 2026, 07:20 UTC">3d ago via GitHub<`, ">waiting 3d<", ">from staging -&gt; prod<"}
	last := -1
	for _, w := range want {
		i := strings.Index(out, w)
		if i < 0 || i < last {
			t.Fatalf("missing or out of order %q in %s", w, out)
		}
		last = i
	}
	if !strings.Contains(out, "&middot;") && !strings.Contains(out, "·") {
		t.Error("no separators")
	}
}
