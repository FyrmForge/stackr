package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A recipe row exports as pg_extensions (a built-in only when switched off),
// and a file whose recipe CheckRecipe refuses blocks the plan, before the diff.
func TestServerFilePGExtensions(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	for _, row := range []store.PGRecipe{
		{ID: "pgvector", Spec: "{}", Enabled: false},
		{ID: "mine", Spec: `{"name":"mine","majors":[17],"packages":["postgresql-{major}-mine"],"extensions":["mine"]}`, Enabled: true, Share: store.NameList{"all"}},
	} {
		if err := r.env.Store.PGRecipes.Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.env.Orch.ExportServerConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pg_extensions:", "pgvector:\n    enabled: false", "mine:", "share: all"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "postgis") {
		t.Errorf("a built-in that is on is not exported:\n%s", out)
	}
	pv, err := r.env.Orch.PreviewServerConfig(ctx, []byte("version: 1\npg_extensions:\n  bad:\n    majors: [17]\n    packages: [a]\n    package_url: https://example.com/x.deb\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Blockers) == 0 {
		t.Errorf("a recipe CheckRecipe refuses did not block: %+v", pv)
	}
}

// A server file that adds an admin recipe and switches a built-in off applies
// through SetPGRecipe and plans clean again.
func TestServerFilePGExtensionsApply(t *testing.T) {
	ctx := context.Background()
	r := newFileRig(t)
	file := "version: 1\npg_extensions:\n  pgvector:\n    enabled: false\n  mine:\n    majors: [17]\n    packages: ['postgresql-{major}-mine']\n    extensions: [mine]\n    share: all\n"
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	vs, err := r.env.Orch.PGRecipes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range vs {
		got[v.Name] = v.Enabled
	}
	if on, ok := got["mine"]; !ok || !on || got["pgvector"] {
		t.Errorf("recipes = %+v", vs)
	}
	again, err := r.env.Orch.PreviewServerConfig(ctx, []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range again.Changes {
		if strings.HasPrefix(c.Kind, "pg-extension") {
			t.Errorf("not clean after apply: %+v", c)
		}
	}
}
