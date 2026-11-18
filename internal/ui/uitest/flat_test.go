package uitest

import "testing"

func TestPanelClassMatch(t *testing.T) {
	for html, want := range map[string]bool{
		`<li class="panel px-4">`:   true,
		`<li class="px-4 panel">`:   true,
		`<li class="py-3 panel-x">`: false,
		`<li class="py-3 text-sm">`: false,
		`<li class="a panel b">`:    true,
	} {
		if got := panelClass.MatchString(html); got != want {
			t.Errorf("%s: %v, want %v", html, got, want)
		}
	}
}
