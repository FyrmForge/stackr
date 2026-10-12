package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// `hamr dev` on the fake host still deploys: the fake never picks a subnet,
// so every network, env, shared and ingress, only exists because the node
// leaf reserved a /24 for it from the controller's range.
func TestFakeHostDeploysOnReservedSubnets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, FakeHost: filepath.Join(dir, "fake")},
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	// service.New wires these; saying it twice is harmless.
	res := node.New(orch.store.Nodes)
	orch.envs.WithSubnets(res).WithPlacement(orch.envPins)
	orch.domains.WithSubnets(res).WithController(orch.controllerID)
	orch.tiles.Gate = tile.Gate{Poll: 5 * time.Millisecond, Grace: 10 * time.Millisecond, Deadline: time.Second}

	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))

	deployIn := func(name string) (store.Environment, store.Tile) {
		t.Helper()
		e, err := orch.CreateEnv(ctx, stack, name, EnvSpec{
			Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main",
		})
		must(t, err)
		tl, err := orch.CreateTile(ctx, Tile{
			StackID: stack, EnvironmentID: e.ID, Name: "web", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80,
		})
		must(t, err)
		j, err := orch.Deploy(ctx, tl.ID)
		must(t, err)
		for range 1000 {
			if j, err = orch.GetJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			if j.State == job.Done || j.State == job.Failed || j.State == job.Cancelled {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if j.State != job.Done {
			t.Fatalf("deploy in %s = %s %q", name, j.State, j.Error)
		}
		e, err = orch.envs.Get(ctx, e.ID)
		must(t, err)
		return e, tl
	}
	dev, devTile := deployIn("dev")
	prod, _ := deployIn("prod")
	if dev.Subnet != "10.213.0.0/24" || prod.Subnet != "10.213.1.0/24" {
		t.Fatalf("env subnets = %q and %q, want the first two /24s of the controller's range", dev.Subnet, prod.Subnet)
	}
	ctl, err := orch.nodeLeaf().Controller(ctx)
	must(t, err)
	if dev.NodeID != ctl.ID || prod.NodeID != ctl.ID {
		t.Fatalf("envs on %q and %q, want the controller %q", dev.NodeID, prod.NodeID, ctl.ID)
	}

	// A domain opens the tile's ingress network, the third allocator: the next block.
	_, err = orch.AttachDomain(ctx, devTile.ID, DomainSpec{Host: "dev.example.com"})
	must(t, err)
	used, err := orch.docker.UsedSubnets(WithNode(ctx, ctl.ID))
	must(t, err)
	var got []string
	for _, p := range used {
		got = append(got, p.String())
	}
	for _, want := range []string{"10.213.0.0/24", "10.213.1.0/24", "10.213.2.0/24"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("no network on %s; the fake host has %v", want, got)
		}
	}
}
