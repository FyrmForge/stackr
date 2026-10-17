package service_test

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// A repo tile in an env built from a branch builds from that branch: an
// empty branch takes the env's, another one is refused (only the env's
// branch's pushes build it).
func TestTileTakesEnvBranch(t *testing.T) {
	r := newSyncRig(t) // staging builds from main
	ctx := context.Background()
	mk := func(name, branch string) (service.Tile, error) {
		return r.e.Orch.CreateTile(ctx, service.Tile{
			StackID:       r.dev.Stack,
			EnvironmentID: r.staging.ID,
			Name:          name,
			Kind:          "service",
			GitURL:        "https://github.com/acme/api",
			GitBranch:     branch,
		})
	}
	tl, err := mk("api", "")
	if err != nil || tl.GitBranch != "main" {
		t.Fatalf("empty branch: %q, %v", tl.GitBranch, err)
	}
	if _, err := mk("web", "feature"); err == nil {
		t.Fatal("a branch other than the env's was taken")
	} else if bad, ok := errs.IsInvalid(err); !ok || bad.Field != "git_branch" {
		t.Fatalf("refusal = %v", err)
	}
	_, _, err = r.e.Orch.UpdateTile(ctx, tl.ID, func(x *service.Tile) error { x.GitBranch = "feature"; return nil })
	if err == nil {
		t.Fatal("an edit to another branch was taken")
	}
}
