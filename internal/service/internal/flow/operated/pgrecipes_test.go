package operated

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// extras beyond the shared `hostile` list: Dockerfile and shell tricks.
var hostileMore = append(slices.Clone(hostile), "a\nUSER root", "a\\\nRUN x", "a && b", "a\r\nRUN x")

func goodURLRecipe() Recipe {
	r := BuiltinRecipes[2]
	r.Name = "mine"
	r.SHA256 = maps.Clone(r.SHA256)
	return r
}

func TestCheckRecipe(t *testing.T) {
	for _, r := range BuiltinRecipes {
		if err := CheckRecipe(r); err != nil {
			t.Errorf("builtin %s: %v", r.Name, err)
		}
	}
	bad := map[string]func(*Recipe){
		"both":          func(r *Recipe) { r.Packages = []string{"a-b"} },
		"neither":       func(r *Recipe) { r.PackageURL = "" },
		"no extensions": func(r *Recipe) { r.Extensions = nil },
		"no majors":     func(r *Recipe) { r.Majors = nil },
		"major 0":       func(r *Recipe) { r.Majors = []int{0} },
		"missing arch":  func(r *Recipe) { delete(r.SHA256, "18-arm64") },
		"no version":    func(r *Recipe) { r.Version = "" },
		"http":          func(r *Recipe) { r.PackageURL = "http://h/x.deb" },
		"unknown hole":  func(r *Recipe) { r.PackageURL = "https://h/{minor}.deb" },
	}
	for name, mut := range bad {
		r := goodURLRecipe()
		mut(&r)
		if _, ok := errs.IsInvalid(CheckRecipe(r)); !ok {
			t.Errorf("%s accepted", name)
		}
	}
}

// Every field that reaches SQL, shell or Dockerfile text refuses hostile input.
func TestCheckRecipeHostile(t *testing.T) {
	fields := map[string]func(*Recipe, string){
		"name":       func(r *Recipe, h string) { r.Name = h },
		"package":    func(r *Recipe, h string) { r.PackageURL = ""; r.Packages = []string{h} },
		"url":        func(r *Recipe, h string) { r.PackageURL = h },
		"url tail":   func(r *Recipe, h string) { r.PackageURL = "https://h/x" + h },
		"version":    func(r *Recipe, h string) { r.Version = h },
		"sha256":     func(r *Recipe, h string) { r.SHA256["17-amd64"] = h },
		"sha key":    func(r *Recipe, h string) { r.SHA256[h] = strings.Repeat("a", 64) },
		"extension":  func(r *Recipe, h string) { r.Extensions = []string{h} },
		"preload":    func(r *Recipe, h string) { r.Preload = []string{h} },
		"requires":   func(r *Recipe, h string) { r.Requires = []string{h} },
		"pkg suffix": func(r *Recipe, h string) { r.PackageURL = ""; r.Packages = []string{"pg-" + h} },
	}
	for field, set := range fields {
		for _, h := range hostileMore {
			r := goodURLRecipe()
			set(&r, h)
			// glued onto a valid prefix these are legal characters
			if (field == "pkg suffix" || field == "url tail") && slices.Contains([]string{"", "-x", "../x", "X"}, h) {
				continue
			}
			if err := CheckRecipe(r); err == nil {
				t.Errorf("%s accepted %q", field, h)
			}
		}
	}
}

func TestResolveRecipes(t *testing.T) {
	rs, err := ResolveRecipes([]string{"vchord"}, 17, "o1", nil)
	if err != nil || len(rs) != 2 || rs[0].Name != "pgvector" || rs[1].Name != "vchord" {
		t.Fatalf("closure %v %v", rs, err)
	}
	if got := DerivedPreload(rs); !slices.Equal(got, []string{"vchord"}) {
		t.Errorf("preload %v", got)
	}
	// extension name, recipe name and a repeat all land on one recipe
	rs, err = ResolveRecipes([]string{"vector", "pgvector", "vchord"}, 17, "o1", nil)
	if err != nil || len(rs) != 2 {
		t.Fatalf("dedupe %v %v", rs, err)
	}
	if rs, err = ResolveRecipes([]string{"postgis_topology"}, 16, "o1", nil); err != nil || rs[0].Name != "postgis" {
		t.Errorf("postgis by extension: %v %v", rs, err)
	}

	refused := func(names []string, major int, rows []store.PGRecipe, want string) {
		t.Helper()
		_, err := ResolveRecipes(names, major, "o1", rows)
		var ref errs.Refused
		if !errors.As(err, &ref) && want != "requires: no recipe ghost" {
			t.Errorf("%v: %v is not a refusal", names, err)
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", names, err, want)
		}
	}
	refused([]string{"nope"}, 17, nil, "nope is not in the image and no recipe has that name.")
	refused([]string{"vchord"}, 16, nil, "the recipe vchord has no build for postgres 16.")
	refused([]string{"vchord"}, 17, []store.PGRecipe{{ID: "pgvector"}}, "the recipe pgvector is switched off on this server.")
	refused([]string{"vector"}, 17, []store.PGRecipe{{ID: "pgvector", Enabled: false}}, "the recipe pgvector is switched off on this server.")

	spec := func(r Recipe) string { b, _ := json.Marshal(r); return string(b) }
	mine := Recipe{Packages: []string{"postgresql-{major}-foo"}, Extensions: []string{"foo"}, Requires: []string{"ghost"}}
	row := store.PGRecipe{ID: "foo", Spec: spec(mine), Enabled: true, Share: store.NameList{"o2"}}
	refused([]string{"foo"}, 17, []store.PGRecipe{row}, "the recipe foo is not shared with o1.")
	row.Share = store.NameList{"all"}
	refused([]string{"foo"}, 17, []store.PGRecipe{row}, "requires: no recipe ghost")
	row.Spec = spec(Recipe{Packages: []string{"postgresql-{major}-foo"}, Extensions: []string{"foo"}, Requires: []string{"pgvector"}})
	if rs, err := ResolveRecipes([]string{"foo"}, 17, "o1", []store.PGRecipe{row}); err != nil || len(rs) != 2 || rs[1].Name != "foo" {
		t.Errorf("admin recipe: %v %v", rs, err)
	}
	row.Share = store.NameList{"o1"}
	if _, err := ResolveRecipes([]string{"foo"}, 17, "o1", []store.PGRecipe{row}); err != nil {
		t.Errorf("shared to the org: %v", err)
	}
	// a row cannot edit a built-in, and a broken row cannot match
	rows := []store.PGRecipe{{ID: "pgvector", Enabled: true, Spec: spec(Recipe{PackageURL: "https://evil/x.deb"})}, {ID: "broken", Spec: "{", Enabled: true, Share: store.NameList{"all"}}}
	if rs, err := ResolveRecipes([]string{"vector"}, 17, "o1", rows); err != nil || rs[0].PackageURL != "" {
		t.Errorf("built-in edited: %v %v", rs, err)
	}
	refused([]string{"broken"}, 17, rows, "no recipe has that name")
	// a cycle does not loop
	a := store.PGRecipe{ID: "ca", Enabled: true, Share: store.NameList{"all"}, Spec: spec(Recipe{Packages: []string{"pa"}, Extensions: []string{"ca"}, Requires: []string{"cb"}})}
	b := store.PGRecipe{ID: "cb", Enabled: true, Share: store.NameList{"all"}, Spec: spec(Recipe{Packages: []string{"pb"}, Extensions: []string{"cb"}, Requires: []string{"ca"}})}
	if rs, err := ResolveRecipes([]string{"ca"}, 17, "o1", []store.PGRecipe{a, b}); err != nil || len(rs) != 2 {
		t.Errorf("cycle: %v %v", rs, err)
	}
}

func TestResolveScriptAndParse(t *testing.T) {
	s := ResolveScript(17, []string{"postgresql-17-pgvector", "postgresql-17-postgis-3"})
	for _, want := range []string{"apt-mark hold postgresql-17 postgresql-client-17", "apt-cache madison", "install -s", "postgresql-17-pgvector postgresql-17-postgis-3"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	for _, h := range hostileMore {
		if slices.Contains([]string{"", "-x"}, h) {
			continue // "pg"+h is a legal name
		}
		got := ResolveScript(17, []string{"postgresql-17-pgvector", "pg" + h})
		if !strings.HasPrefix(got, "echo 'err") || strings.Contains(got, "madison") {
			t.Errorf("hostile package %q produced a real script", h)
		}
	}

	out := "ok postgresql-17-pgvector 0.8.6-1.pgdg13+2 0.8.0-1\nnone postgresql-17-foo 17.12\n\n"
	r, err := ParseResolve(out)
	if err != nil || r.Versions["postgresql-17-pgvector"] != "0.8.6-1.pgdg13+2" || r.NeedMinor != "17.12" || !slices.Equal(r.None, []string{"postgresql-17-foo"}) {
		t.Fatalf("parse: %+v %v", r, err)
	}
	for _, bad := range []string{"", "err apt-get update failed", "ok p", "ok p 1.0\nbogus", "ok p 1.0;rm", "ok P 1.0", "none p 17.1 x", "ok p -x", "ok pg$(id) 1.0"} {
		if _, err := ParseResolve(bad); err == nil {
			t.Errorf("ParseResolve(%q) accepted", bad)
		}
	}
	if r, err := ParseResolve("none postgresql-17-foo $(id)\n"); err != nil || r.NeedMinor != "" {
		t.Errorf("hostile minor kept: %+v %v", r, err)
	}
}

func TestRecipeDockerfile(t *testing.T) {
	rs, _ := ResolveRecipes([]string{"vchord"}, 17, "o1", nil)
	pins := map[string]string{"postgresql-17-pgvector": "0.8.6-1.pgdg13+2"}
	df, err := RecipeDockerfile("postgres:17.11", 17, "amd64", rs, pins)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"FROM postgres:17.11\n", "LABEL stackr.image=pgext\n",
		"ADD --checksum=sha256:a48132c8d616c0d52a9811b32f12f88a8f6cc8542d7cc0180c8d0986bfbd03c5 https://github.com/supervc-stack/VectorChord/releases/download/1.1.1/postgresql-17-vchord_1.1.1-1_amd64.deb /tmp/ext-1.deb\n",
		"apt-mark hold postgresql-17 postgresql-client-17", "postgresql-17-pgvector=0.8.6-1.pgdg13+2",
		"apt-get install -y --no-install-recommends /tmp/ext-1.deb", "&& postgres --version\n",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("dockerfile lacks %q:\n%s", want, df)
		}
	}
	if strings.Count(df, "\n") != 4 {
		t.Errorf("unexpected shape:\n%s", df)
	}
	if arm, _ := RecipeDockerfile("postgres:17.11", 17, "arm64", rs, pins); !strings.Contains(arm, "_arm64.deb") || !strings.Contains(arm, "50ae776b") {
		t.Error("arm64 deb and checksum not used")
	}

	for _, c := range []struct {
		name string
		call func() (string, error)
	}{
		{"no pin", func() (string, error) { return RecipeDockerfile("postgres:17.11", 17, "amd64", rs, nil) }},
		{"major", func() (string, error) { return RecipeDockerfile("postgres:16.4", 16, "amd64", rs, pins) }},
		{"cpu", func() (string, error) { return RecipeDockerfile("postgres:17.11", 17, "riscv64", rs, pins) }},
	} {
		if _, err := c.call(); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
	for _, h := range hostileMore {
		pin := func(v string) map[string]string { return map[string]string{"postgresql-17-pgvector": v} }
		if _, err := RecipeDockerfile(h, 17, "amd64", rs, pins); err == nil {
			t.Errorf("base %q accepted", h)
		}
		if _, err := RecipeDockerfile("postgres:17.11", 17, h, rs, pins); err == nil {
			t.Errorf("arch %q accepted", h)
		}
		if _, err := RecipeDockerfile("postgres:17.11", 17, "amd64", rs, pin(h)); err == nil {
			t.Errorf("pin %q accepted", h)
		}
		// recipe fields reach the Dockerfile too, and are checked again here
		evil := slices.Clone(rs)
		evil[1].Version = h
		evil[0].Packages = []string{"pg" + h}
		if _, err := RecipeDockerfile("postgres:17.11", 17, "amd64", evil[1:], pins); err == nil {
			t.Errorf("version %q accepted", h)
		}
		if _, err := RecipeDockerfile("postgres:17.11", 17, "amd64", evil[:1], pins); err == nil {
			t.Errorf("package %q accepted", h)
		}
	}
	// a sha256 that is not hex never gets written
	bad := slices.Clone(rs)
	bad[1].SHA256 = maps.Clone(bad[1].SHA256)
	bad[1].SHA256["17-amd64"] = "x\nRUN evil"
	if _, err := RecipeDockerfile("postgres:17.11", 17, "amd64", bad, pins); err == nil {
		t.Error("hostile sha256 accepted")
	}
}

func TestBuildTag(t *testing.T) {
	d := "FROM x\n"
	tag := BuildTag("postgres:17.11", 0, "amd64", d)
	if !strings.HasPrefix(tag, "17.11-ext-amd64-") || len(tag) != len("17.11-ext-amd64-")+8 {
		t.Errorf("tag %q", tag)
	}
	if BuildTag("postgres:17.11", 0, "amd64", d) != tag || BuildTag("postgres:17.11", 0, "amd64", d+" ") == tag || BuildTag("postgres:17.11", 0, "arm64", d) == tag {
		t.Error("tag is not a function of the dockerfile and arch")
	}
	if got := BuildTag("reg.example:5000/lib/postgres:18.7-bookworm", 0, "arm64", d); !strings.HasPrefix(got, "18.7-ext-arm64-") {
		t.Errorf("registry base: %q", got)
	}
	if got := BuildTag("postgres@sha256:abc", 0, "a$md64", d); !strings.HasPrefix(got, "unknown-ext-amd64-") {
		t.Errorf("digest base: %q", got)
	}
}

// A tag without a minor still names the major, and the same recipes and pins
// make the same Dockerfile and tag in any order.
func TestBuildTagMajorAndOrder(t *testing.T) {
	d := "FROM x\n"
	for _, base := range []string{"postgres:17", "postgres@sha256:abc", "postgres"} {
		tag := BuildTag(base, 17, "amd64", d)
		if !strings.HasPrefix(tag, "17-ext-amd64-") || ImageMajor("reg/org/pg:"+tag) != 17 {
			t.Errorf("%s: tag %q", base, tag)
		}
	}
	if !strings.HasPrefix(BuildTag("postgres:17.11", 17, "amd64", d), "17.11-ext-") {
		t.Error("a minor in the base is lost")
	}
	gis, _ := ResolveRecipes([]string{"postgis"}, 17, "o1", nil)
	vch, _ := ResolveRecipes([]string{"vchord"}, 17, "o1", nil)
	pins := map[string]string{"postgresql-17-pgvector": "0.8.6-1", "postgresql-17-postgis-3": "3.5-1", "postgresql-17-postgis-3-scripts": "3.5-1"}
	a, err := RecipeDockerfile("postgres:17.11", 17, "amd64", slices.Concat(gis, vch), pins)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RecipeDockerfile("postgres:17.11", 17, "amd64", slices.Concat(vch, gis), pins)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || BuildTag("postgres:17.11", 17, "amd64", a) != BuildTag("postgres:17.11", 17, "amd64", b) {
		t.Errorf("order changed the build:\n%s\n---\n%s", a, b)
	}
}
