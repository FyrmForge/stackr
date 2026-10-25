package canvas

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
)

// The field marks at an env: Changed where the env sets a value, From the
// nearest rung that does when it does not, nothing on a built-in default,
// and the pair rule tags the user with the rung that set the password.
func TestCascadeRowsMarks(t *testing.T) {
	knobs := []service.Knob{
		{Key: "cpu_limit", Default: "0"},
		{Key: "mem_limit_mb", Default: "0"},
		{Key: "protect", Default: "false"},
		{Key: "protect_user"},
		{Key: "protect_password"},
		{Key: "workers", Default: "4"}, // not a cascade knob
	}
	above := []rung{
		{"shop stack", "stack", `{"mem_limit_mb":512}`},
		{"acme organization", "org", `{"cpu_limit":2,"protect_password":"pw"}`},
		{"server", "server", `{"protect":true}`},
	}
	rows, err := cascadeRows(knobs, `{"mem_limit_mb":256}`, above)
	if err != nil {
		t.Fatal(err)
	}
	type mark struct {
		changed   bool
		from, eff string
	}
	want := map[string]mark{
		"cpu_limit":        {false, "org", "2 cores"},
		"mem_limit_mb":     {true, "", "512 MB"},
		"protect":          {false, "server", "true"},
		"protect_user":     {false, "org", ""},
		"protect_password": {false, "", "not set"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if w := want[r.Key]; r.Changed != w.changed || r.From != w.from || r.Effective != w.eff {
			t.Errorf("%s = changed %v from %q effective %q, want %+v", r.Key, r.Changed, r.From, r.Effective, w)
		}
	}

	rows, _ = cascadeRows(knobs, `{"protect_password":"x","protect_user":"u"}`, nil)
	for _, r := range rows {
		switch r.Key {
		case "cpu_limit":
			if r.Changed || r.From != "" || r.Effective != "no limit" {
				t.Errorf("built-in default = %+v, want no marks", r)
			}
		case "protect_password":
			if !r.Changed || r.Value != "" || r.Effective != "set" {
				t.Errorf("own password = %+v, want changed and never echoed", r)
			}
		}
	}
}

func TestCascadeSummary(t *testing.T) {
	rows := []comp.SettingRowView{
		{Key: "cpu_limit", Effective: "2 cores", From: "org"},
		{Key: "mem_limit_mb", Value: "512", Effective: "1024 MB"},
		{Key: "protect", Effective: "false"},
		{Key: "protect_user", Value: "bob"},
	}
	if got, want := cascadeSummary(rows), "2 cores (from org) · 512 MB · no password"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	rows = []comp.SettingRowView{
		{Key: "cpu_limit", Effective: "no limit"},
		{Key: "protect", Effective: "true", From: "server"},
	}
	if got, want := cascadeSummary(rows), "no CPU limit · password protected (from server)"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
