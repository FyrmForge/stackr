package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Settings on a running postgres instance: a reload when every changed one
// takes effect by it, a restart job when one needs it, a refusal for an
// owned name or one postgres does not know, and nothing stored on a refusal.
func TestPGSettingsApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := servicetest.New(t)
	org := env.Org(t, "acme")
	tl := env.Tile(t, org)
	o := env.Orch
	pg, err := o.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	env.Docker.Containers = append(env.Docker.Containers, docker.Container{
		ID:     "pg1",
		State:  "running",
		Labels: map[string]string{"stackr.tile": pg.ID, "stackr.role": "replica"},
	})
	known := map[string]string{"work_mem": "user", "max_connections": "postmaster", "statement_timeout": "user"}
	env.Docker.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		if !strings.Contains(j, "name || '|' || context FROM pg_settings") {
			return ""
		}
		var out []string
		for n, c := range known {
			if strings.Contains(j, "'"+n+"'") {
				out = append(out, n+"|"+c)
			}
		}
		return strings.Join(out, "\n")
	}

	// reloadable: no job, stored
	v, j, err := o.SetPGSettings(ctx, pg.ID, map[string]string{"work_mem": "8MB"}, nil)
	if err != nil || j != nil {
		t.Fatalf("work_mem: job %v, err %v", j, err)
	}
	var got string
	for _, s := range v.Settings {
		if s.Name == "work_mem" {
			got = s.Value + "/" + s.Source
		}
	}
	if got != "8MB/yours" {
		t.Errorf("work_mem = %s", got)
	}
	var reloaded, wrote bool
	for _, c := range env.Docker.Calls() {
		reloaded = reloaded || strings.Contains(strings.Join(c.Args, " "), "pg_reload_conf()")
		wrote = wrote || strings.Contains(strings.Join(c.Args, " "), "stackr.conf.new")
	}
	if !reloaded || !wrote {
		t.Errorf("reload %v, atomic write %v", reloaded, wrote)
	}

	// a postmaster setting: restarts
	_, j, err = o.SetPGSettings(ctx, pg.ID, map[string]string{"max_connections": "200", "work_mem": "16MB"}, nil)
	if err != nil || j == nil {
		t.Fatalf("max_connections: job %v, err %v", j, err)
	}

	// refused: owned, unknown, empty; nothing stored
	for name, set := range map[string]map[string]string{
		"owned":   {"wal_level": "logical"},
		"unknown": {"nonsense": "1"},
		"empty":   {"work_mem": ""},
	} {
		if _, _, err := o.SetPGSettings(ctx, pg.ID, set, nil); err == nil {
			t.Errorf("%s: no error", name)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("%s: err = %v, want invalid", name, err)
		}
	}
	m, err := o.PGSettings(ctx, pg.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Settings {
		if s.Name == "nonsense" || s.Name == "wal_level" {
			t.Errorf("a refused setting is listed: %+v", s)
		}
	}

	// unset: back to the default, no longer yours
	v, _, err = o.SetPGSettings(ctx, pg.ID, nil, []string{"work_mem"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range v.Settings {
		if s.Name == "work_mem" && s.Source == "yours" {
			t.Errorf("work_mem is still yours after an unset")
		}
	}
	// a slice tile's owner names nothing to set on an image tile
	if _, _, err := o.SetPGSettings(ctx, tl.ID, map[string]string{"work_mem": "8MB"}, nil); err == nil {
		t.Error("settings on an image tile")
	}
}
