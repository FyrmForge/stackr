package service_test

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A rename carries every reference that names the renamed slug, so a slice
// still resolves after its env, stack, org or instance tile is renamed; a
// stack file's reference is left alone and comes back as a warning.
func TestRenameCarriesSliceRefs(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	org := env.Org(t, "acme")
	tl := env.Tile(t, org)
	o := env.Orch
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	pg, err := o.CreateManagedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres")
	must(err)
	_, err = o.SetManagedAllow(ctx, pg.ID, []string{"acme:shop:dev:web", "acme:*:dev:*"})
	must(err)
	_, err = o.SetManagedEnvPairs(ctx, pg.ID, map[string]string{"dev": "dev"})
	must(err)
	db, err := o.CreateSliceTile(ctx, tl.Env, "db", "shop:dev:pg", "")
	must(err)
	healthy := func(step, from string, allow []string, pairs map[string]string) {
		t.Helper()
		v, err := o.SliceOf(ctx, db.ID)
		must(err)
		if v.Blocker != "" || v.ProvisionFrom != from {
			t.Errorf("%s: slice = %q, blocker %q; want %q, healthy", step, v.ProvisionFrom, v.Blocker, from)
		}
		ms, err := o.ManagedInstances(ctx, tl.Env)
		must(err)
		if len(ms) != 1 || !slices.Equal(ms[0].Allow, allow) || len(ms[0].EnvPairs) != len(pairs) {
			t.Fatalf("%s: instances = %+v", step, ms)
		}
		for k, v := range pairs {
			if ms[0].EnvPairs[k] != v {
				t.Errorf("%s: env pairs = %v, want %v", step, ms[0].EnvPairs, pairs)
			}
		}
	}
	healthy("before", "shop:dev:pg", []string{"acme:shop:dev:web", "acme:*:dev:*"}, map[string]string{"dev": "dev"})

	_, warn, err := o.RenameEnv(ctx, tl.Env, "Bad Name!")
	must(err)
	if len(warn) != 0 {
		t.Errorf("env rename warnings = %v", warn)
	}
	healthy("env", "shop:bad-name:pg", []string{"acme:shop:bad-name:web", "acme:*:dev:*"}, map[string]string{"bad-name": "bad-name"})

	_, _, err = o.RenameStack(ctx, tl.Stack, "Store")
	must(err)
	healthy("stack", "store:bad-name:pg", []string{"acme:store:bad-name:web", "acme:*:dev:*"}, map[string]string{"bad-name": "bad-name"})

	_, _, err = o.RenameOrg(ctx, org, "Globex")
	must(err)
	healthy("org", "store:bad-name:pg", []string{"globex:store:bad-name:web", "globex:*:dev:*"}, map[string]string{"bad-name": "bad-name"})

	_, _, err = o.RenameTile(ctx, pg.ID, "main")
	must(err)
	healthy("tile", "store:bad-name:main", []string{"globex:store:bad-name:web", "globex:*:dev:*"}, map[string]string{"bad-name": "bad-name"})

	// A config-managed stack's slice is its file's: kept as written, warned.
	_, err = o.SetConfigRepo(ctx, tl.Stack, env.Connector(t, org, "whsec"), "acme/shop", "", "")
	must(err)
	_, warn, err = o.RenameEnv(ctx, tl.Env, "qa")
	must(err)
	if len(warn) != 2 { // the slice and the instance's allow and pairs
		t.Errorf("config-managed rename warnings = %v, want 2", warn)
	}
	v, err := o.SliceOf(ctx, db.ID)
	must(err)
	if v.ProvisionFrom != "store:bad-name:main" {
		t.Errorf("config-managed slice = %q, want it as written", v.ProvisionFrom)
	}
}
