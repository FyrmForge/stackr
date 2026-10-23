package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
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

// A push queues a push job only for a stack with an env on the pushed
// branch; another branch of the same repo queues none.
func TestPushSkipsUnfollowedBranch(t *testing.T) {
	ctx := context.Background()
	e := servicetest.New(t)
	org := e.Org(t, "acme")
	seed := e.Tile(t, org) // shop/dev builds from main
	conn := e.Connector(t, org, "sec")
	if _, err := e.Orch.CreateTile(ctx, service.Tile{
		StackID: seed.Stack, EnvironmentID: seed.Env, Name: "web", Kind: "service",
		GitURL: "https://github.com/acme/web",
	}); err != nil {
		t.Fatal(err)
	}
	push := func(branch string) int {
		body := []byte(`{"ref":"refs/heads/` + branch + `","after":"abc",` +
			`"repository":{"clone_url":"https://github.com/acme/web.git","default_branch":"main"},` +
			`"commits":[{"modified":["x"]}]}`)
		mac := hmac.New(sha256.New, []byte("sec"))
		mac.Write(body)
		if err := e.Orch.Webhook(ctx, conn, hook("push", "sha256="+hex.EncodeToString(mac.Sum(nil))), body); err != nil {
			t.Fatal(err)
		}
		js, err := e.Orch.Jobs(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, j := range js {
			if j.Kind == "push" {
				n++
			}
		}
		return n
	}
	if n := push("feature"); n != 0 {
		t.Fatalf("push jobs after a push to an unfollowed branch = %d", n)
	}
	if n := push("main"); n != 1 {
		t.Fatalf("push jobs after a push to main = %d, want 1", n)
	}
}

// Moving an env to another branch takes its building tiles along, so the
// new branch's pushes build them.
func TestEnvBranchMovesTiles(t *testing.T) {
	r := newSyncRig(t) // staging builds from main
	ctx := context.Background()
	tl, err := r.e.Orch.CreateTile(ctx, service.Tile{
		StackID: r.dev.Stack, EnvironmentID: r.staging.ID, Name: "api",
		Kind: "service", GitURL: "https://github.com/acme/api",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Orch.SetEnvFrom(ctx, r.staging.ID, "branch", "release", false); err != nil {
		t.Fatal(err)
	}
	ts, err := r.e.Orch.Tiles(ctx, r.staging.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.ID == tl.ID && x.GitBranch != "release" {
			t.Fatalf("tile branch after env move = %q", x.GitBranch)
		}
	}
}
