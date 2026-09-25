package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// DECIDE 189: a push to a bound stack's config branch makes the envs the
// file's ladder names and the stack lacks; dev tracks main auto, prod
// promotes, and the push's release lands in dev. A second push makes
// nothing, and an env the file drops stays.
func TestPushMakesLadderEnvs(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	conn := env.Connector(t, org, "whsec")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetConfigRepo(ctx, st.ID, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	push := func(file string) {
		t.Helper()
		sha := g.Commit(t, "acme/shop", "main", map[string]string{"stackr-compose.yml": file})
		body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `",` +
			`"repository":{"clone_url":"https://github.com/acme/shop.git","default_branch":"main"},` +
			`"commits":[{"modified":["stackr-compose.yml"]}]}`)
		mac := hmac.New(sha256.New, []byte("whsec"))
		mac.Write(body)
		if err := env.Orch.Webhook(ctx, conn, "push", "sha256="+hex.EncodeToString(mac.Sum(nil)), body); err != nil {
			t.Fatal(err)
		}
	}
	ladder := func() []service.Environment {
		t.Helper()
		es, err := env.Orch.Ladder(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return es
	}
	releases := func() int {
		rs, err := env.Orch.Releases(ctx, st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}

	push("version: 1\nstack: shop\nladder:\n  - dev\n  - prod\nhead: main\n")
	eventually(t, "a release in dev", func() bool {
		es := ladder()
		return len(es) == 2 && es[0].ReleaseID != nil
	})
	dev, prod := ladder()[0], ladder()[1]
	if dev.Slug != "dev" || dev.FromKind != "branch" || dev.FromBranch != "main" || !dev.Auto {
		t.Errorf("dev = %s %s %s auto=%v, want dev tracking main auto", dev.Slug, dev.FromKind, dev.FromBranch, dev.Auto)
	}
	if prod.Slug != "prod" || prod.FromKind != "promote" || prod.ReleaseID != nil {
		t.Errorf("prod = %s %s release=%v, want prod promoting, nothing in it", prod.Slug, prod.FromKind, prod.ReleaseID)
	}

	push("version: 1\nstack: shop\nladder:\n  - dev\nhead: main\n")
	eventually(t, "the second release", func() bool { return releases() == 2 })
	es := ladder()
	ids := func(es []service.Environment) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	if !slices.Equal(ids(es), []string{dev.ID, prod.ID}) {
		t.Errorf("after the second push the ladder is %+v, want dev and prod as they were", es)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
