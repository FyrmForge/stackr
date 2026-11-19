package render

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
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
func TestRelRowFailedPromoteKeepsMessage(t *testing.T) {
	r := service.WindowRow{Kind: service.RowFailed, State: service.StateFailed, Number: 19, Message: "fix login", FailEnv: "dev", Err: "a\ndeploy web: boom.", JobID: "j"}
	got := RelRowOf(r, "shop", nil, nil, func(id string) string { return "/jobs/" + id })
	if got.Message != "fix login" || got.Err != "deploy web: boom" {
		t.Errorf("message %q err %q", got.Message, got.Err)
	}
}
