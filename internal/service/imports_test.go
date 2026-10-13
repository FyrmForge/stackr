package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The contract wiring: Import goes to the registry leaf (still a stub, so
// unsupported), the push callback tolerates every org, and recordImage writes
// through the leaf.
func TestImportContract(t *testing.T) {
	ctx := context.Background()
	orch, err := New(
		Config{DataDir: t.TempDir(), SecretsKey: testKey, Conntrack: "/nonexistent"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	if _, _, err := orch.Import(ctx, "nope", "postgres:16", "", nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("import for an unknown org: %v, want ErrNotFound", err)
	}
	og := store.Org{ID: "o1", Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: time.Now()}
	must(t, orch.store.Orgs.Create(ctx, og))
	if _, _, err := orch.Import(ctx, og.ID, "postgres:16", "", nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("import = %v, want the leaf's unsupported", err)
	}
	for _, repo := range []string{"acme/shop/web", "ghost/shop/web", regfront.SystemOrg + "/tools/rsync"} {
		orch.registryPushed(ctx, regfront.Pushed{Repo: repo, Reference: "v1", Digest: "sha256:aa", Kind: regfront.KindUser, Subject: "u1"})
	}

	img := store.RegistryImage{OrgID: &og.ID, Repo: "acme/shop/web", Tag: "v1", Digest: "sha256:aa", Source: "built", CreatedAt: time.Now()}
	must(t, orch.recordImage(ctx, img))
	if rows, err := orch.store.RegistryImages.ListByOrg(ctx, og.ID); err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}
