package service_test

import (
	"context"
	"strings"
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
// secret is never echoed nor cleared by an empty field. The server file's
// binding is the Config tab's, not a row here.
func TestServerSettings(t *testing.T) {
	env := servicetest.New(t)
	orch, ctx := env.Orch, context.Background()
	all := map[string]string{}
	for k, s := range byKey(t, orch) {
		all[k] = s.Value
	}
	for k := range all {
		if strings.HasPrefix(k, "server_config_") || k == "dns_env" {
			t.Errorf("the Settings tab lists %s", k)
		}
	}
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	if d, _ := orch.SettingDefaults(ctx); d.JSON() != "{}" {
		t.Errorf("an untouched form wrote the server rung: %s", d.JSON())
	}
	all["workers"], all["mem_limit_mb"] = "4", "512"
	all["protect_user"], all["protect_password"] = "u", "hunter2"
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	got := byKey(t, orch)
	if got["workers"].Value != "4" || got["mem_limit_mb"].Value != "512" || got["mem_limit_mb"].DecidedBy != "server" {
		t.Errorf("not written: %+v %+v", got["workers"], got["mem_limit_mb"])
	}
	if s := got["protect_password"]; s.Value != "" || s.Effective != "set" {
		t.Errorf("secret echoed: %+v", s)
	}
	all["protect_password"] = ""
	if err := orch.SetServerSettings(ctx, all); err != nil {
		t.Fatal(err)
	}
	if d, _ := orch.SettingDefaults(ctx); d.ProtectPassword == nil || *d.ProtectPassword != "hunter2" {
		t.Errorf("an empty secret field cleared it: %+v", d.ProtectPassword)
	}
	all["workers"] = "x"
	if err := orch.SetServerSettings(ctx, all); err == nil {
		t.Error("a bad value was taken")
	}
}
