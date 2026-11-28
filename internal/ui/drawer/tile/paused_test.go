package tile

import (
	"context"
	"strings"
	"testing"
)

func render(t *testing.T, v View) string {
	var b strings.Builder
	if err := actionButtons(v).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// A Docker-paused tile offers no Start, Stop or Restart; a stopped one
// still offers Start and a running one Restart.
func TestPausedOffersNoLifecycle(t *testing.T) {
	v := View{Kind: "service", Name: "api", Base: "/b", Status: "paused"}
	got := render(t, v)
	for _, no := range []string{"/start", "/stop", "/restart"} {
		if strings.Contains(got, no) {
			t.Errorf("paused offers %s", no)
		}
	}
	if !strings.Contains(got, "/deploy") {
		t.Error("paused lost Deploy")
	}
	v.Status, v.Stopped = "stopped", true
	if !strings.Contains(render(t, v), "/start") {
		t.Error("stopped lost Start")
	}
	v.Status, v.Stopped = "running", false
	if !strings.Contains(render(t, v), "/restart") {
		t.Error("running lost Restart")
	}
}
