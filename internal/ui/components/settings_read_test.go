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

// A control carries the stored value, so a form redrawn after a refused save
// (refused values as defaults) still tells a held edit from the stored value.
func TestSettingsFormReadModeStoredValue(t *testing.T) {
	row := SettingRowView{Key: "workers", Label: "Job workers", Type: "int", Value: "4"}
	html := render(t, SettingsForm(SettingsFormView{ID: "f", Action: "/a", Rows: []SettingRowView{row}}))
	if !strings.Contains(html, `data-stored="4"`) {
		t.Errorf("no stored value on the control:\n%s", html)
	}
}

// The pending footer is the card's flush footer inside a Section, a plain
// divided strip elsewhere; never a rounded tinted box.
func TestSettingsFormPendingFooterFlush(t *testing.T) {
	row := SettingRowView{Key: "workers", Label: "Job workers", Type: "int", Value: "4"}
	flush := render(t, SettingsForm(SettingsFormView{ID: "f", Action: "/a", Flush: true, Rows: []SettingRowView{row}}))
	plain := render(t, SettingsForm(SettingsFormView{ID: "f", Action: "/a", Rows: []SettingRowView{row}}))
	if !strings.Contains(flush, "-mx-4 sm:-mx-6 -mb-4 sm:-mb-6") || strings.Contains(flush, "rounded-lg bg-rw-inset") {
		t.Errorf("flush footer:\n%s", flush)
	}
	if strings.Contains(plain, "-mx-4") || strings.Contains(plain, "rounded-lg bg-rw-inset") {
		t.Errorf("plain footer:\n%s", plain)
	}
}
