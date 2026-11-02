package service_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A dump stackr does not load is refused before anything is made; a good
// one makes the instance and its slice, and queues one seed job that has
// the dump on disk until it ends.
func TestCreateManagedTileFromDump(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	org := env.Org(t, "acme")
	tl := env.Tile(t, org)
	o := env.Orch
	in := service.Tile{EnvironmentID: tl.Env, Name: "pg"}

	tiles := func() int {
		ts, err := o.Tiles(ctx, tl.Env)
		if err != nil {
			t.Fatal(err)
		}
		return len(ts)
	}
	before := tiles()
	for name, c := range map[string]struct {
		engine, dump, field string
	}{
		"gzip":  {"postgres", "\x1f\x8b\x08\x00", "dump"},
		"empty": {"postgres", "", "dump"},
		"s3":    {"s3", "PGDMP", "engine"},
	} {
		_, _, err := o.CreateManagedTileFromDump(ctx, in, c.engine, "", strings.NewReader(c.dump))
		if bad, ok := errs.IsInvalid(err); !ok || bad.Field != c.field {
			t.Errorf("%s: err = %v, want invalid %s", name, err, c.field)
		}
	}
	if n := tiles(); n != before {
		t.Fatalf("a refused dump made %d tiles", n-before)
	}

	dump := bytes.Repeat([]byte("PGDMP"), 100_000) // past any one buffer
	out, j, err := o.CreateManagedTileFromDump(ctx, in, "postgres", "", bytes.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "managed" || j.Kind != "seed" {
		t.Errorf("tile %s, job %s", out.Kind, j.Kind)
	}
	ts, err := o.Tiles(ctx, tl.Env)
	if err != nil {
		t.Fatal(err)
	}
	var slice service.Tile
	for _, x := range ts {
		if x.Slug == "pg-db" {
			slice = x
		}
	}
	if slice.Kind != "slice" || slice.ProvisionFrom == nil || *slice.ProvisionFrom != "shop:dev:pg" {
		t.Fatalf("slice = %+v, want pg-db cut from shop:dev:pg", slice)
	}
	if !slices.Contains(j.LockSet, out.ID) || !slices.Contains(j.LockSet, slice.ID) {
		t.Errorf("lock set %q holds not both tiles", j.LockSet)
	}

	// the job fails (the fake runs no postgres) and takes the dump with it
	seeds := filepath.Join(env.Config.DataDir, "seeds")
	eventually(t, "the seed job to end", func() bool {
		got, err := o.GetJob(ctx, j.ID)
		return err == nil && got.State != "queued" && got.State != "running"
	})
	if got, _ := o.GetJob(ctx, j.ID); got.State != "failed" {
		t.Errorf("seed job = %s, want failed", got.State)
	}
	if left, _ := os.ReadDir(seeds); len(left) != 0 {
		t.Errorf("%d dumps left in %s", len(left), seeds)
	}
}

// A seed job cancelled while it is still queued takes its dump along.
func TestCancelledSeedDropsDump(t *testing.T) {
	ctx := context.Background()
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	// a running upgrade helper holds the queue: the seed stays queued
	env.Orch.SetUpgradeHeld(true)
	_, j, err := env.Orch.CreateManagedTileFromDump(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres", "",
		strings.NewReader("PGDMP"))
	if err != nil {
		t.Fatal(err)
	}
	seeds := filepath.Join(env.Config.DataDir, "seeds")
	if left, _ := os.ReadDir(seeds); len(left) != 1 {
		t.Fatalf("%d dumps spooled, want 1", len(left))
	}
	if err := env.Orch.CancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.Orch.GetJob(ctx, j.ID); got.State != "cancelled" {
		t.Fatalf("seed job = %s, want cancelled before it ran", got.State)
	}
	if left, _ := os.ReadDir(seeds); len(left) != 0 {
		t.Errorf("%d dumps left after the cancel", len(left))
	}
}
