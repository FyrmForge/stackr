package components

import (
	"strings"
	"testing"
)

// A knob shows as a fact with a pencil; its control sits behind it, closed;
// the default tag marks a value nobody set; the footer is there for Apply.
func TestSettingsFormReadMode(t *testing.T) {
	html := render(t, SettingsForm(SettingsFormView{
		ID: "jobs", Action: "/admin/jobs", Flat: true,
		Rows: []SettingRowView{
			{Key: "workers", Label: "Job workers", Type: "int", Effective: "2", DecidedBy: "default", NoInherit: true},
			{Key: "backup_run_concurrency", Label: "Backups at once", Type: "int", Value: "4", Effective: "4", DecidedBy: "server", Changed: true},
			{Key: "protect_password", Label: "Password", Secret: true, Effective: "set"},
		},
	}))
	for _, want := range []string{
		"<settings-read", `aria-label="Edit Job workers"`, `data-label="Job workers"`, `data-editor hidden`,
		">default<", ">Apply<", "data-pending", "data-cancel-all", `name="workers"`, ">Set<",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
	if strings.Count(html, ">default<") != 1 {
		t.Error("the default tag is on a row somebody set")
	}
}

// A refused row opens its control at once; a read-only form has no pencil.
func TestSettingsFormReadModeRefusedAndReadOnly(t *testing.T) {
	row := SettingRowView{Key: "workers", Label: "Job workers", Type: "int", Value: "x", Error: "a number"}
	if html := render(t, SettingsForm(SettingsFormView{ID: "f", Action: "/a", Rows: []SettingRowView{row}})); !strings.Contains(html, "<div data-editor>") {
		t.Errorf("a refused row stays closed:\n%s", html)
	}
	if html := render(t, SettingsForm(SettingsFormView{ID: "f", ReadOnly: true, Why: "no", Rows: []SettingRowView{row}})); strings.Contains(html, "data-edit ") || strings.Contains(html, "data-pending") {
		t.Errorf("a read-only form offers an edit:\n%s", html)
	}
}
