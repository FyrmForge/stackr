package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// checkWorld is a release host that counts its hits.
func checkWorld(t *testing.T, version string) (*world, *atomic.Int32) {
	t.Helper()
	w := newWorld(t)
	var hits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = rw.Write([]byte(`{"tag_name":"v0.9.0"}`))
	}))
	t.Cleanup(gh.Close)
	dg := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte("sha256:" + strings.Repeat("a", 64)))
	}))
	t.Cleanup(dg.Close)
	w.orch.upgrade.URL, w.orch.upgrade.Version, w.orch.upgrade.DigestFmt = gh.URL, version, dg.URL+"/%s"
	return w, &hits
}

func stampChecked(t *testing.T, w *world, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	if err := w.orch.store.Settings.Set(context.Background(), keyUpgradeChecked, at); err != nil {
		t.Fatal(err)
	}
}

// The automatic check runs when the last one is older than the interval (the
// default is 6h), never when it is fresh, and the interval counts from the
// stored time, so a restart neither resets nor skips it.
func TestAutoUpgradeCheckInterval(t *testing.T) {
	t.Parallel()
	w, hits := checkWorld(t, "v0.8.1")
	ctx := context.Background()

	w.orch.autoUpgradeCheck(ctx) // never checked: due at boot
	if hits.Load() != 1 {
		t.Fatalf("boot with no check on record: %d hits, want 1", hits.Load())
	}
	if w.orch.UpgradeCheckedAt(ctx).IsZero() {
		t.Fatal("the check did not stamp its time")
	}
	w.orch.autoUpgradeCheck(ctx) // fresh: nothing, however often the tick calls
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 1 {
		t.Fatalf("fresh check re-ran: %d hits", hits.Load())
	}
	stampChecked(t, w, 5*time.Hour) // a restart 5h on: still inside the 6h default
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 1 {
		t.Fatalf("5h old with the 6h default ran: %d hits", hits.Load())
	}
	stampChecked(t, w, 7*time.Hour)
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 2 {
		t.Fatalf("7h old with the 6h default: %d hits, want 2", hits.Load())
	}
	if err := w.orch.settings.Set(ctx, "update_check", "weekly"); err != nil {
		t.Fatal(err)
	}
	stampChecked(t, w, 3*24*time.Hour)
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 2 {
		t.Fatalf("3 days old with weekly ran: %d hits", hits.Load())
	}
	stampChecked(t, w, 8*24*time.Hour)
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 3 {
		t.Fatalf("8 days old with weekly: %d hits, want 3", hits.Load())
	}
}

// off never checks by itself; a manual check still works and stamps its time.
func TestAutoUpgradeCheckOffAndManual(t *testing.T) {
	t.Parallel()
	w, hits := checkWorld(t, "v0.8.1")
	ctx := context.Background()
	if err := w.orch.settings.Set(ctx, "update_check", "off"); err != nil {
		t.Fatal(err)
	}
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 0 {
		t.Fatalf("off checked: %d hits", hits.Load())
	}
	stampChecked(t, w, 30*24*time.Hour)
	before := w.orch.UpgradeCheckedAt(ctx)
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 0 {
		t.Fatalf("off checked a month on: %d hits", hits.Load())
	}
	if _, _, err := w.orch.CheckUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	if !w.orch.UpgradeCheckedAt(ctx).After(before) {
		t.Error("a manual check did not stamp its time")
	}
	if got := w.orch.UpgradeCheckedText(ctx); got != "Checked under a minute ago" {
		t.Errorf("text = %q", got)
	}
}

// A dev build does nothing, and has no checked time to show.
func TestAutoUpgradeCheckDevBuild(t *testing.T) {
	t.Parallel()
	w, hits := checkWorld(t, "dev")
	ctx := context.Background()
	w.orch.autoUpgradeCheck(ctx)
	if hits.Load() != 0 || !w.orch.UpgradeCheckedAt(ctx).IsZero() {
		t.Fatalf("dev build checked: %d hits", hits.Load())
	}
	if got := w.orch.UpgradeCheckedText(ctx); got != "Never checked" {
		t.Errorf("text = %q", got)
	}
}

// update_check takes the five values and refuses the rest; empty is the default.
func TestUpdateCheckSetting(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	if v, _ := w.orch.Setting(ctx, "update_check"); v != "6h" {
		t.Errorf("default = %q, want 6h", v)
	}
	for _, ok := range []string{"off", "1h", "6h", "daily", "weekly", ""} {
		if _, err := w.orch.SetServerSettings(ctx, map[string]string{"update_check": ok}); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"2h", "never", "Daily", "6"} {
		if _, err := w.orch.SetServerSettings(ctx, map[string]string{"update_check": bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
