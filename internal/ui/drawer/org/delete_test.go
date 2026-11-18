package org

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// A refused org delete (the last org, say) shows its reason in the open
// Delete row, outside the closed dialog.
func TestRefusedDeleteShowsReason(t *testing.T) {
	const msg = "The last organization cannot be deleted."
	ctx := c.WithRefused(t.Context(), &c.Refused{Row: "delete", Msg: msg})
	ctx = c.WithPosted(ctx, "/acme/-/drawer/delete")
	var b strings.Builder
	v := SettingsView{Name: "acme", Slug: "acme", Rename: "/acme/-/drawer/rename",
		Delete: c.ConfirmView{Button: "Delete org", Title: "Delete acme?", Word: "acme", Kept: []string{"x"}, Action: "/acme/-/drawer/delete", Target: "#drawer-root"}}
	if err := Settings(v).Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	i := strings.Index(out, msg)
	if i < 0 {
		t.Fatalf("no reason in\n%s", out)
	}
	if strings.Count(out[:i], "<dialog") != strings.Count(out[:i], "</dialog>") {
		t.Errorf("the reason sits inside the closed dialog\n%s", out[i-300:i+100])
	}
	if !strings.Contains(out, `data-acc="delete" open`) {
		t.Errorf("the Delete row is not open\n%s", out)
	}
}

// The only org on the server says up front that it cannot be deleted, with
// no button that would be refused.
func TestLastOrgSaysItCannotBeDeleted(t *testing.T) {
	var b strings.Builder
	v := SettingsView{Name: "acme", Slug: "acme", Rename: "/acme/-/drawer/rename", Last: true,
		Delete: c.ConfirmView{Button: "Delete org", Title: "Delete acme?", Word: "acme", Kept: []string{"x"}, Action: "/d"}}
	if err := Settings(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "The last organization cannot be deleted.") || strings.Contains(out, "<confirm-dialog") {
		t.Errorf("last org delete\n%s", out)
	}
}
