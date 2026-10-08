package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// proxy_custom reaches the pushed proxy config; a value that is not a JSON
// array of routes is refused before anything is written or pushed.
func TestProxyCustomPushed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	route := `{"match":[{"host":["extra.example.com"]}],"handle":[{"handler":"static_response","body":"hi"}]}`
	must(t, w.orch.SetSetting(ctx, "proxy_custom", "["+route+"]"))
	if !strings.Contains(w.lastPush(), "extra.example.com") {
		t.Fatalf("pushed config lacks the custom route:\n%s", w.lastPush())
	}
	before := w.lastPush()
	if err := w.orch.SetSetting(ctx, "proxy_custom", "{not an array"); err == nil {
		t.Fatal("bad proxy_custom saved")
	} else if _, ok := errs.IsInvalid(err); !ok {
		t.Errorf("err = %v, want Invalid", err)
	}
	if v, _ := w.orch.Setting(ctx, "proxy_custom"); !strings.Contains(v, "extra.example.com") || w.lastPush() != before {
		t.Error("a refused save changed the stored value or pushed")
	}
	must(t, w.orch.SetSetting(ctx, "proxy_custom", ""))
	if strings.Contains(w.lastPush(), "extra.example.com") {
		t.Error("clearing proxy_custom left the route in the config")
	}
}
