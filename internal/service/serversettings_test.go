package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func byKey(t *testing.T, orch *service.Orchestrator) map[string]service.ServerSetting {
	t.Helper()
	ss, err := orch.ServerSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]service.ServerSetting{}
	for _, s := range ss {
		m[s.Key] = s
	}
	return m
}

// The admin form posts every row: only changed values are written, and a
// secret is never echoed nor cleared by an empty field.
func TestServerSettings(t *testing.T) {
	env := servicetest.New(t)
	orch, ctx := env.Orch, context.Background()
	all := map[string]string{}
	for k, s := range byKey(t, orch) {
		all[k] = s.Value
	}
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	if d, _ := orch.SettingDefaults(ctx); d.JSON() != "{}" {
		t.Errorf("an untouched form wrote the server rung: %s", d.JSON())
	}
	all["workers"], all["mem_limit_mb"], all["dns_env"] = "4", "512", "TOKEN=x"
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	got := byKey(t, orch)
	if got["workers"].Value != "4" || got["mem_limit_mb"].Value != "512" || got["mem_limit_mb"].DecidedBy != "server" {
		t.Errorf("not written: %+v %+v", got["workers"], got["mem_limit_mb"])
	}
	if s := got["dns_env"]; s.Value != "" || s.Effective != "set" {
		t.Errorf("secret echoed: %+v", s)
	}
	all["dns_env"] = ""
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	if v, _ := orch.Setting(ctx, "dns_env"); v != "TOKEN=x" {
		t.Errorf("an empty secret field cleared it: %q", v)
	}
	all["workers"] = "x"
	if err := orch.SetServerSettings(ctx, all); err == nil {
		t.Error("a bad value was taken")
	}
}
