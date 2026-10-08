package settings_test

import (
	"context"
	"slices"
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

// root_domain is the installer's boot value until somebody sets it; the
// server file may set it too (the rename itself is the service's).
func TestRootDomainSettable(t *testing.T) {
	l := settings.New(servicetest.Store(t).Settings, map[string]string{"root_domain": "example.com"})
	ctx := context.Background()
	if err := l.Set(ctx, "root_domain", "other.com"); err != nil {
		t.Fatalf("set root_domain: %v", err)
	}
	if got, _ := l.Get(ctx, "root_domain"); got != "other.com" {
		t.Errorf("root_domain = %q, want the row", got)
	}
	if err := l.Set(ctx, "root_domain", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get(ctx, "root_domain"); got != "example.com" {
		t.Errorf("root_domain = %q, want the boot value back", got)
	}
}

// The server file's binding is five flat knobs the Settings tab skips; the
// dead dns_env knob is gone.
func TestServerConfigKnobs(t *testing.T) {
	want := []string{
		"server_config_connector", "server_config_repo", "server_config_branch",
		"server_config_path", "server_config_auto",
	}
	for _, k := range settings.Catalogue {
		if k.Key == "dns_env" {
			t.Error("dns_env is still in the catalogue; nothing reads it")
		}
		if i := slices.Index(want, k.Key); i >= 0 {
			want = slices.Delete(want, i, i+1)
			if k.Scopes != settings.Flat || !k.ConfigOnly {
				t.Errorf("%s: scopes %v config-only %v, want a flat config-only knob", k.Key, k.Scopes, k.ConfigOnly)
			}
		} else if k.ConfigOnly {
			t.Errorf("%s is config-only but not a server_config knob", k.Key)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing knobs %v", want)
	}
	l := settings.New(servicetest.Store(t).Settings, nil)
	ctx := context.Background()
	if got, err := l.Get(ctx, "server_config_auto"); err != nil || got != "false" {
		t.Errorf("server_config_auto = %q, %v; want false by default", got, err)
	}
	if err := l.Set(ctx, "server_config_auto", "maybe"); err == nil {
		t.Error("server_config_auto took a non-bool")
	}
	if err := l.Set(ctx, "server_config_repo", "acme/server"); err != nil {
		t.Errorf("server_config_repo: %v", err)
	}
}

// proxy_custom is a JSON array of Caddy route objects; anything else is
// refused at save, so a typo never reaches the proxy.
func TestProxyCustomChecked(t *testing.T) {
	for _, bad := range []string{`{"a":1}`, `[1]`, `[`, `"x"`, `[{"a":1}] x`} {
		if err := settings.Check("proxy_custom", bad); err == nil {
			t.Errorf("proxy_custom %q accepted", bad)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("proxy_custom %q: %v, want Invalid", bad, err)
		}
	}
	for _, ok := range []string{"", "[]", ` [ {"match":[{"host":["a.io"]}]} ] `} {
		if err := settings.Check("proxy_custom", ok); err != nil {
			t.Errorf("proxy_custom %q refused: %v", ok, err)
		}
	}
}
