package admin

import (
	"strings"
	"testing"
)

// The off-box warning shows while NoOffBox is set and is gone otherwise.
func TestBackupsOffBoxWarning(t *testing.T) {
	for _, tc := range []struct{ no, want bool }{{true, true}, {false, false}} {
		var b strings.Builder
		if err := Backups(BackupsView{NoOffBox: tc.no}).Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), "Backups are only on this server. "); got != tc.want {
			t.Errorf("NoOffBox=%v: warning shown = %v", tc.no, got)
		}
	}
}
