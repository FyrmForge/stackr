package settings_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Install-wide knobs: a settings row beats the boot value beats the
// catalogue default (DECIDE 15); a typo is refused, never read as unset.
func TestFlat(t *testing.T) {
	l := settings.New(servicetest.Store(t).Settings, map[string]string{"orphan_retention_days": "7"})
	ctx := context.Background()

	check := func(key string, want int) {
		t.Helper()
		if got, err := l.Int(ctx, key); err != nil || got != want {
			t.Errorf("%s = %d, %v; want %d", key, got, err, want)
		}
	}
	check("workers", 2)               // catalogue default
	check("orphan_retention_days", 7) // boot value over the default
	if err := l.Set(ctx, "orphan_retention_days", "14"); err != nil {
		t.Fatal(err)
	}
	check("orphan_retention_days", 14) // row over the boot value
	for _, bad := range []string{"4 0", "-1", "0", "four"} {
		if err := l.Set(ctx, "workers", bad); err == nil {
			t.Errorf("workers %q accepted", bad)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("workers %q: %v, want Invalid", bad, err)
		}
	}
	check("workers", 2)
	if err := l.Set(ctx, "image_check_interval", "0"); err != nil {
		t.Errorf("image_check_interval 0 (off) refused: %v", err)
	}
	if err := l.Set(ctx, "orphan_retention_days", ""); err != nil {
		t.Fatal(err)
	}
	check("orphan_retention_days", 7)

	if err := l.SetDefaults(ctx, map[string]string{"mem_limit_mb": "256"}); err != nil {
		t.Fatal(err)
	}
	if err := l.SetDefaults(ctx, map[string]string{"mem_limit_mb": "2 56"}); err == nil {
		t.Error("typo in the server rung accepted")
	}
	d, err := l.Defaults(ctx)
	if err != nil || settings.Resolve(d).MemLimitMB != 256 {
		t.Errorf("server rung = %+v, %v", d, err)
	}
}

// root_domain is the installer's: read from the boot value, never written.
func TestRootDomainReadOnly(t *testing.T) {
	l := settings.New(servicetest.Store(t).Settings, map[string]string{"root_domain": "example.com"})
	ctx := context.Background()
	for _, v := range []string{"other.com", ""} {
		err := l.Set(ctx, "root_domain", v)
		invalid, ok := errs.IsInvalid(err)
		if !ok || invalid.Field != "root_domain" {
			t.Errorf("set root_domain %q: %v, want Invalid", v, err)
		}
	}
	got, err := l.Get(ctx, "root_domain")
	if err != nil || got != "example.com" {
		t.Errorf("root_domain = %q, %v; want the boot value", got, err)
	}
}
