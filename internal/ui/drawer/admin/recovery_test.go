package admin

import (
	"strings"
	"testing"
)

// An unsaved recovery key shows the warning and the password form; the key
// itself appears only on the answer to the check, with the confirm button.
func TestRecoverySection(t *testing.T) {
	render := func(v RecoveryView) string {
		var b strings.Builder
		if err := Recovery(v).Render(t.Context(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	unsaved := render(RecoveryView{Gen: 1, Unsaved: true, Show: "/s", Saved: "/v"})
	for _, want := range []string{"data-recovery-warning", `type="password"`, "Show the key"} {
		if !strings.Contains(unsaved, want) {
			t.Errorf("unsaved section lacks %q", want)
		}
	}
	if strings.Contains(unsaved, "AGE-SECRET-KEY") || strings.Contains(unsaved, "Rotate the key") {
		t.Error("an unsaved section showed the key or offered rotation")
	}
	shown := render(RecoveryView{Gen: 1, Unsaved: true, Key: "AGE-SECRET-KEY-X", Saved: "/v"})
	if !strings.Contains(shown, "AGE-SECRET-KEY-X") || !strings.Contains(shown, "saved it") {
		t.Errorf("revealed section:\n%s", shown)
	}
	saved := render(RecoveryView{Gen: 2, Rotate: "/r"})
	if strings.Contains(saved, "data-recovery-warning") || !strings.Contains(saved, "Rotate the key") {
		t.Errorf("saved section:\n%s", saved)
	}
}
