package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A new postgres instance has its primary member, no standby, and on a
// single node is told to add a node; a standby on its own node is refused.
func TestProtectionSingleNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := servicetest.New(t)
	tl := env.Tile(t, env.Org(t, "acme"))
	pg, err := env.Orch.CreateOperatedTile(ctx, service.Tile{EnvironmentID: tl.Env, Name: "pg"}, "postgres", 0)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := env.Orch.InstanceMembers(ctx, pg.ID)
	if err != nil || len(ms) != 1 || ms[0].Role != "primary" {
		t.Fatalf("members = %+v %v", ms, err)
	}
	if p, err := env.Orch.Protection(ctx, pg.ID); err != nil || p != "No standby. Add a node to get a standby." {
		t.Errorf("protection = %q %v", p, err)
	}
}
