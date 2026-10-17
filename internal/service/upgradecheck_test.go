package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The last check outlives a restart: the Update tab reads it back.
func TestLastUpgradeCheckRemembered(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	gh := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte(`{"tag_name":"v0.9.0"}`))
	}))
	defer gh.Close()
	w.orch.upgrade.URL, w.orch.upgrade.Version = gh.URL, "v0.8.1"

	if _, _, ok := w.orch.LastUpgradeCheck(ctx); ok {
		t.Fatal("a check is remembered before any ran")
	}
	if _, _, err := w.orch.CheckUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if latest, newer, ok := w.orch.LastUpgradeCheck(ctx); !ok || latest != "v0.9.0" || !newer {
		t.Fatalf("last check = %q newer=%v ok=%v", latest, newer, ok)
	}
	// Once on that release, the same answer reads as up to date.
	w.orch.upgrade.Version = "v0.9.0"
	if _, newer, _ := w.orch.LastUpgradeCheck(ctx); newer {
		t.Error("v0.9.0 is newer than itself")
	}
}
