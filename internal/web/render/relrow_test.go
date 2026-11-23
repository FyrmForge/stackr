package render

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

func TestRelPlanRowViewOnly(t *testing.T) {
	r := service.WindowRow{Kind: service.RowPlan, State: service.StateWaiting}
	if got := RelPlanRow(r, "org config", "/x", true).Link.Label; got != "View plan" {
		t.Fatalf("viewer label = %q", got)
	}
	if got := RelPlanRow(r, "org config", "/x", false).Link.Label; got != "Review plan" {
		t.Fatalf("owner label = %q", got)
	}
}

// An unnamed user shows as "a member", not as their id.
func TestSourceWordUnnamedUser(t *testing.T) {
	if got := SourceWord("user:abc", map[string]string{"x": "Nia"}); got != "a member" {
		t.Errorf("got %q", got)
	}
	if got := SourceWord("user:x", map[string]string{"x": "Nia"}); got != "Nia" {
		t.Errorf("got %q", got)
	}
}

// A failed release keeps its commit message; the cause is its own line.
func TestReleaseLineFailedPromoteKeepsMessage(t *testing.T) {
	r := service.WindowRow{Kind: service.RowFailed, State: service.StateFailed, Number: 19, Message: "fix login", By: "push", Commit: "abcdef0123", FailEnv: "dev", Err: "a\ndeploy web: boom.", JobID: "j"}
	got := ReleaseLineOf(r, "shop", "/o/shop", nil, nil, func(id string) string { return "/jobs/" + id })
	if got.Message != "fix login" || got.Err != "deploy web: boom" || got.Commit != "abcdef0" {
		t.Errorf("message %q err %q commit %q", got.Message, got.Err, got.Commit)
	}
}

// Only a push shows a commit; the others say what cut them; a waiting row
// says where it goes and opens the modal of its env.
func TestReleaseLineKinds(t *testing.T) {
	envs := map[string]components.EnvRun{"e1": {Name: "prod", Color: "teal"}}
	job := func(id string) string { return "/jobs/" + id }
	rb := service.WindowRow{Kind: service.RowRelease, ID: "r2", Number: 24, By: "rebuild", Commit: "abcdef0123", Cause: "Rebuild of release 23", OpenEnv: "prod"}
	got := ReleaseLineOf(rb, "shop", "/o/shop", nil, envs, job)
	if got.Commit != "" || got.Cause != "Rebuild of release 23" || got.Open != "/o/shop/prod/-/releases/r2/dialog" {
		t.Errorf("rebuild = %+v", got)
	}
	iw := service.WindowRow{Kind: service.RowRelease, ID: "r3", Number: 25, By: "image-watch", Cause: "api 1.0 to 1.1", State: service.StateWaiting, WaitEnvs: []string{"e1"}}
	got = ReleaseLineOf(iw, "shop", "/o/shop", nil, envs, job)
	if got.Change != "api 1.0 to 1.1" || got.Cause != "" || got.To != "prod" || got.Source != "image watch" {
		t.Errorf("image watch = %+v", got)
	}
	iw.NoVersion = true
	if got = ReleaseLineOf(iw, "shop", "/o/shop", nil, envs, job); !got.NoVersion {
		t.Errorf("unread version not carried: %+v", got)
	}
	push := service.WindowRow{Kind: service.RowRelease, ID: "r4", Number: 26, By: "push", Commit: "abcdef0123", Message: "fix login", OpenEnv: "prod"}
	got = ReleaseLineOf(push, "shop", "/o/shop", nil, envs, job)
	if got.Commit != "abcdef0" || got.Message != "fix login" {
		t.Errorf("push = %+v", got)
	}
	b := ReleaseLineOf(service.WindowRow{Kind: service.RowBuilding, Commit: "abcdef0123", By: "push"}, "shop", "/o/shop", nil, envs, job)
	if b.Cause != "Building at abcdef0" || b.Open != "" || b.Link.Label != "Job log" || b.State != "building" {
		t.Errorf("building = %+v", b)
	}
	if q := ReleaseLineOf(service.WindowRow{Kind: service.RowBuilding, Queued: true, By: "push"}, "shop", "/o/shop", nil, envs, job); q.State != "queued" {
		t.Errorf("queued build state = %q", q.State)
	}
	if a := RelPlanRow(service.WindowRow{State: service.StateWaiting}, "org config", "/x", false).Avatar; a.Icon != "github" {
		t.Errorf("a plan row needs a mark, got %+v", a)
	}
}

// The modal view carries the service's groups, the state chip tone and the
// changes with Old and New.
func TestReleaseModalOfGroups(t *testing.T) {
	d := service.ReleaseDetail{ID: "r", Number: 3, State: service.StateWaiting, Impacts: []string{"i"}, Gaps: []string{"k not set"},
		Changes: []service.ReleaseChange{{Label: "api image", Old: "1", New: "2"}}}
	v := ReleaseModalOf(d, "/o/s/e", true, false, nil)
	if v.State != "waiting" || v.Tone != "accent" || len(v.Impacts) != 1 || v.Gaps[0] != "k not set" ||
		len(v.Changes) != 1 || v.Changes[0].Old != "1" || v.Changes[0].New != "2" {
		t.Errorf("view = %+v", v)
	}
}

func TestReleaseModalOfLine2(t *testing.T) {
	d := service.ReleaseDetail{By: "image-watch", At: time.Now().Add(-2 * time.Hour)}
	v := ReleaseModalOf(d, "/o/s/e", false, false, nil)
	if v.Via != "image watch" || v.Age != "2h ago" {
		t.Errorf("via %q age %q", v.Via, v.Age)
	}
}
