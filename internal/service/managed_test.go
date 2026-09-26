package service_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Step 7b task 6: allow and env pairs on a managed tile, a slice tile cut
// from it, a consumer's access re-granted in place, and the slice's
// bindings without a secret.
func TestSliceVerbs(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	org := env.Org(t, "acme")
	tl := env.Tile(t, org)
	o := env.Orch
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	invalid := func(what string, err error) {
		t.Helper()
		if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("%s = %v, want invalid", what, err)
		}
	}
	pg, err := o.CreateManagedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres")
	must(t, err)

	_, err = o.SetManagedAllow(ctx, pg.ID, []string{"other:shop:*"})
	invalid("allow in another org", err)
	_, err = o.SetManagedAllow(ctx, tl.ID, []string{"acme:*"})
	invalid("allow on an image tile", err)
	m, err := o.SetManagedAllow(ctx, pg.ID, []string{"acme:shop:*"})
	must(t, err)
	if !slices.Equal(m.Allow, []string{"acme:shop:*"}) {
		t.Errorf("allow = %v", m.Allow)
	}
	_, err = o.SetManagedEnvPairs(ctx, pg.ID, map[string]string{"staging": "prod"})
	invalid("pair to a missing env", err)
	m, err = o.SetManagedEnvPairs(ctx, pg.ID, map[string]string{
		"dev":     "dev",
		"staging": "dev",
	})
	must(t, err)
	if m.EnvPairs["staging"] != "dev" {
		t.Errorf("env pairs = %v", m.EnvPairs)
	}

	_, err = o.CreateSliceTile(ctx, tl.Env, "bad", "shop:dev", "")
	invalid("two-segment provision_from", err)
	db, err := o.CreateSliceTile(ctx, tl.Env, "db", "shop:dev:pg", "")
	must(t, err)
	if db.Kind != "slice" || *db.DefaultAccess != "write" || *db.OnRemove != "keep" {
		t.Errorf("slice tile = %+v", db)
	}
	v, err := o.SliceOf(ctx, db.ID)
	must(t, err)
	if v.Target != "shop:dev:pg" || v.Blocker != "" || v.Provisioned || v.Network != "stackr-managed-"+m.ID {
		t.Errorf("slice view = %+v", v)
	}
	far, err := o.CreateSliceTile(ctx, tl.Env, "far", "shop:nope:pg", "read")
	must(t, err)
	if v, err := o.SliceOf(ctx, far.ID); err != nil || v.Blocker == "" || v.Target != "" {
		t.Errorf("unresolved slice view = %+v, %v", v, err)
	}
	if bs, err := o.Bindings(ctx, db.ID); err != nil || len(bs) != 0 {
		t.Errorf("bindings before a deploy = %v, %v", bs, err)
	}

	_, err = o.SetSliceAccess(ctx, pg.ID, "db", "read")
	invalid("access for a managed tile", err)
	_, err = o.SetSliceAccess(ctx, tl.ID, "pg", "read")
	invalid("access to a managed tile", err)
	_, err = o.SetSliceAccess(ctx, tl.ID, "nope", "read")
	invalid("access to a missing slice", err)

	// api holds a write cred on db; read re-grants it in place.
	p := store.Provision{
		ID:         uuid.NewString(),
		TileID:     db.ID,
		InstanceID: m.ID,
		DBName:     "shop_dev_db",
		DBUser:     "shop_dev_db",
		CreatedAt:  time.Now(),
	}
	must(t, env.Store.Provisions.Create(ctx, p))
	must(t, env.Store.Bindings.Create(ctx, store.Binding{
		ID:             uuid.NewString(),
		ProvisionID:    p.ID,
		ConsumerTileID: tl.ID,
		Access:         "write",
		DBUser:         "shop_dev_db_api",
		DBPassword:     "LEAK-cred",
		Outputs:        `{"PGPASSWORD":"LEAK-cred"}`,
		CreatedAt:      time.Now(),
	}))
	env.Docker.Containers = append(env.Docker.Containers, docker.Container{
		ID:    "pg1",
		State: "running",
		Labels: map[string]string{
			"stackr.tile": pg.ID,
			"stackr.role": "replica",
		},
	})
	env.Docker.ExecOut = "0"
	api, err := o.SetSliceAccess(ctx, tl.ID, "db", "read")
	must(t, err)
	if !slices.Equal(api.SliceAccess, []store.SliceAccess{{From: "db", Access: "read"}}) {
		t.Errorf("slice_access = %v", api.SliceAccess)
	}
	var ex strings.Builder
	for _, c := range env.Docker.Calls() {
		if c.Method == "Exec" {
			ex.WriteString(strings.Join(c.Args, " "))
		}
	}
	if !strings.Contains(ex.String(), `REVOKE ALL ON DATABASE "shop_dev_db" FROM "shop_dev_db_api"`) {
		t.Errorf("no re-grant in\n%s", ex.String())
	}
	bs, err := o.Bindings(ctx, db.ID)
	must(t, err)
	if len(bs) != 1 || bs[0].Consumer != "api" || bs[0].Kind != "image" || bs[0].Access != "read" || bs[0].User != "shop_dev_db_api" {
		t.Errorf("bindings = %+v", bs)
	}
	v, err = o.SliceOf(ctx, db.ID)
	must(t, err)
	if !v.Provisioned || v.Name != "shop_dev_db" || v.Network != "stackr-managed-"+m.ID {
		t.Errorf("provisioned slice view = %+v", v)
	}

	cb, err := o.ConsumerBindings(ctx, tl.ID)
	must(t, err)
	if len(cb) != 1 || cb[0].SliceID != db.ID || cb[0].Slice != "db" || cb[0].Access != "read" {
		t.Errorf("consumer bindings = %+v", cb)
	}
	_, on, err := o.InstanceSlices(ctx, pg.ID)
	must(t, err)
	if len(on) != 1 || on[0].Slice != "db" || on[0].Stack != "shop" || on[0].Env != "dev" || on[0].Bindings != 1 {
		t.Errorf("instance slices = %+v", on)
	}
	db, err = o.SetSliceDefaultAccess(ctx, db.ID, "read")
	must(t, err)
	if *db.DefaultAccess != "read" {
		t.Errorf("default_access = %v", *db.DefaultAccess)
	}
	_, err = o.SetSliceDefaultAccess(ctx, db.ID, "admin")
	invalid("default_access admin", err)

	_, err = o.SetSliceOnRemove(ctx, db.ID, "later")
	invalid("on_remove later", err)
	_, err = o.SetSliceOnRemove(ctx, tl.ID, "drop")
	invalid("on_remove on an image tile", err)
	db, err = o.SetSliceOnRemove(ctx, db.ID, "drop")
	must(t, err)
	if *db.OnRemove != "drop" {
		t.Errorf("on_remove = %v", *db.OnRemove)
	}

	// A config-managed stack declares its slices in the file.
	conn := env.Connector(t, org, "whsec")
	_, err = o.SetConfigRepo(ctx, tl.Stack, conn, "acme/shop", "", "")
	must(t, err)
	if _, err := o.CreateSliceTile(ctx, tl.Env, "late", "shop:dev:pg", ""); err == nil {
		t.Error("slice tile on a config-managed stack was made")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("config-managed = %v, want conflict", err)
	}
	if _, err := o.SetSliceDefaultAccess(ctx, db.ID, "write"); err == nil {
		t.Error("default_access on a config-managed stack was set")
	}
}
