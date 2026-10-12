package access

import (
	"context"
	"strings"
	"testing"
)

// The controller warning shows with the move link, and the form still has
// its Approve button: it warns, it does not block.
func TestApproveControllerWarning(t *testing.T) {
	render := func(w *ControllerWarn) string {
		var b strings.Builder
		v := ApproveView{ID: "a", Action: "/approve", Name: "shop", Pending: []string{"web network:host"},
			Tiles: Group(nil, []string{"web network:host"}), Controller: w}
		if err := Approve(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	got := render(&ControllerWarn{Node: "home", Envs: []ControllerEnv{{Name: "production", Move: "/o/s/production/-/drawer"}}})
	for _, want := range []string{"runs on home, the controller", "Run it on a worker", `hx-get="/o/s/production/-/drawer"`, "Approve"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
	if strings.Contains(render(nil), "the controller") {
		t.Error("warns with nothing on the controller")
	}
}
