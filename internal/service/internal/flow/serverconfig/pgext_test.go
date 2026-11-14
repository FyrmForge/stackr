package serverconfig_test

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
)

const pgAdmin = "pg_extensions:\n  mine:\n    majors: [17]\n    packages: [\"postgresql-{major}-mine\"]\n    extensions: [mine]\n"

func withRecipes(l *serverconfig.Live) {
	l.PGRecipes = []serverconfig.RecipeLive{
		{Name: "pgvector", Builtin: true, Enabled: true},
		{Name: "postgis", Builtin: true, Enabled: true},
		{
			Name: "mine", Enabled: true, ShareOrgs: []string{"o1"},
			Conf: serverconfig.RecipeConf{Majors: []int{17}, Packages: []string{"postgresql-{major}-mine"}, Extensions: []string{"mine"}},
		},
	}
}

func TestDiffPGExtensions(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		mut        func(*serverconfig.Live)
		changes    []string
		blocker    string
	}{
		{name: "no block says nothing", file: "", mut: withRecipes},
		{name: "same is clean", file: pgAdmin + "    share: [acme]\n", mut: withRecipes},
		{name: "add", file: pgAdmin + "    share: all\n", changes: []string{"pg-extension:recipe", "pg-extension:share"}},
		{name: "change a package", file: strings.Replace(pgAdmin, "mine]", "mine2]", 1), mut: withRecipes, changes: []string{"pg-extension:recipe"}},
		{name: "reshare", file: pgAdmin + "    share: [acme, globex]\n", mut: withRecipes, changes: []string{"pg-extension:share"}},
		{name: "switch off", file: pgAdmin + "    enabled: false\n", mut: withRecipes, changes: []string{"pg-extension:enabled"}},
		{name: "switch a built-in off", file: "pg_extensions:\n  postgis: {enabled: false}\n", mut: withRecipes, changes: []string{"pg-extension:enabled", "pg-extension-delete:recipe"}},
		{name: "built-in already on", file: "pg_extensions:\n  postgis: {enabled: true}\n", mut: withRecipes, changes: []string{"pg-extension-delete:recipe"}},
		{name: "built-in cannot be edited", file: "pg_extensions:\n  postgis: {version: \"1\"}\n", mut: withRecipes, blocker: "built in"},
		{name: "unknown org", file: pgAdmin + "    share: [nobody]\n", mut: withRecipes, blocker: "no organization"},
		{name: "refused definition", file: pgAdmin, mut: func(l *serverconfig.Live) {
			l.RecipeErrors = map[string]string{"mine": "set packages or package_url, not both"}
		}, blocker: "not both"},
		{name: "unnamed is an unticked removal", file: "pg_extensions: {}\n", mut: withRecipes, changes: []string{"pg-extension-delete:recipe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mut []func(*serverconfig.Live)
			if tc.mut != nil {
				mut = append(mut, tc.mut)
			}
			p := diff(t, tc.file, mut...)
			if tc.blocker != "" {
				if !p.Blocked() || !strings.Contains(strings.Join(p.Blockers, ";"), tc.blocker) {
					t.Errorf("blockers = %q, want %q", p.Blockers, tc.blocker)
				}
				return
			}
			wantKinds(t, p, tc.changes...)
			for _, c := range p.Changes {
				if (c.Kind == "pg-extension-delete") != c.Optional {
					t.Errorf("only the removal is optional: %+v", c)
				}
			}
		})
	}
}

func TestParsePGExtensionsRefuses(t *testing.T) {
	for _, body := range []string{
		"pg_extensions:\n  Bad Name: {}\n",
		"pg_extensions:\n  mine: {share: [Not_A_Slug]}\n",
		"pg_extensions:\n  mine: {nope: 1}\n",
	} {
		if _, err := serverconfig.Parse([]byte(v1 + body)); err == nil {
			t.Errorf("parsed %q", body)
		}
	}
}

func TestExportPGExtensionsThenDiffIsClean(t *testing.T) {
	l := full()
	withRecipes(&l)
	l.PGRecipes[1].Enabled = false                             // postgis off
	l.PGRecipes = append(l.PGRecipes, serverconfig.RecipeLive{ // an admin recipe, off, shared with all
		Name: "idle", Enabled: false, ShareAll: true,
		Conf: serverconfig.RecipeConf{Majors: []int{18}, PackageURL: "https://example.com/{version}/{arch}.deb", Version: "1.0", Extensions: []string{"idle"},
			SHA256: map[string]string{"18-amd64": strings.Repeat("a", 64)}},
	})
	out, err := serverconfig.Export(l)
	if err != nil {
		t.Fatal(err)
	}
	f, err := serverconfig.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p := serverconfig.Diff(f, l); len(p.Changes) != 0 || p.Blocked() {
		t.Errorf("export diffs against itself: %+v\n%s", p, out)
	}
	if strings.Contains(string(out), "pgvector") || !strings.Contains(string(out), "postgis") {
		t.Errorf("a built-in is exported only when off:\n%s", out)
	}
	// nothing set: no block at all
	if out, _ := serverconfig.Export(full()); strings.Contains(string(out), "pg_extensions") {
		t.Errorf("empty export names recipes:\n%s", out)
	}
}
