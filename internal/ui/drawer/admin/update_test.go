package admin

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

func renderUpdate(t *testing.T, v UpdateView) string {
	t.Helper()
	var b strings.Builder
	if err := Update(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A worker on the panel's build says "matches the panel" while a panel
// update is pending, and "up to date" otherwise.
func TestWorkerChipCopy(t *testing.T) {
	r := WorkerRow{Name: "h4", Current: "1.0"}
	for _, tc := range []struct {
		row   WorkerRow
		newer bool
		want  string
	}{
		{r, true, "matches the panel"},
		{r, false, "up to date"},
		{WorkerRow{Name: "h3", Behind: true}, true, "behind"},
		{WorkerRow{Name: "h3", State: "done"}, true, "done"},
	} {
		if got := workerState(tc.row, tc.newer); got != tc.want {
			t.Errorf("%+v newer=%v: %q, want %q", tc.row, tc.newer, got, tc.want)
		}
	}
	v := UpdateView{Version: "1.0", Checked: true, Latest: "v1.1", Newer: true, Workers: &WorkersView{Rows: []WorkerRow{r}}}
	if out := renderUpdate(t, v); !strings.Contains(out, "matches the panel") || strings.Contains(out, "up to date") {
		t.Errorf("pending update: chip copy wrong\n%s", out)
	}
}

// The tab says when it last checked, and shows the update_check select with
// the current choice picked.
func TestUpdateCheckedAndSetting(t *testing.T) {
	v := UpdateView{
		Version: "1.0", Checked: true, Latest: "v1.0", CheckedText: "Checked 5 min ago",
		Settings: c.SettingsFormView{
			ID: "admin-update", Action: "/-/admin/settings/update", Flat: true, Keys: []string{"update_check"},
			Rows: []c.SettingRowView{{
				Key: "update_check", Label: "Check for stackr updates", Type: "string", Value: "daily", NoInherit: true,
				Options: []c.Option{{Value: "off", Label: "off"}, {Value: "6h", Label: "6h"}, {Value: "daily", Label: "daily"}},
			}},
		},
	}
	out := renderUpdate(t, v)
	for _, want := range []string{"Checked 5 min ago", `name="update_check"`, `<option value="daily" selected>`, `hx-post="/-/admin/settings/update"`} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, `<option value="off" selected>`) {
		t.Errorf("off is picked too\n%s", out)
	}
	out = renderUpdate(t, UpdateView{Version: "1.0", CheckedText: "Never checked"})
	if !strings.Contains(out, "Never checked") || strings.Contains(out, "<select") {
		t.Errorf("never checked tab wrong\n%s", out)
	}
}
