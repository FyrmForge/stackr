package operated

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func planIn() PlanHashIn {
	return PlanHashIn{
		Names: []string{"vchord", "pgvector"},
		Recipes: []Recipe{
			{Name: "vchord", Majors: []int{18, 17}, PackageURL: "https://h/{version}/{major}-{arch}.deb",
				SHA256:  map[string]string{"17-amd64": strings.Repeat("a", 64), "18-amd64": strings.Repeat("b", 64)},
				Version: "1.1.1", Preload: []string{"vchord"}, Requires: []string{"pgvector"}},
			{Name: "pgvector", Packages: []string{"postgresql-{major}-pgvector"}},
		},
		Pins:       map[string]string{"postgresql-17-pgvector": "0.8.0-1", "postgresql-17": "17.11-1"},
		Base:       "postgres:17.11",
		Arch:       "amd64",
		NodeID:     "n1",
		ImageRef:   "postgres:17.11",
		Preload:    []string{"vchord"},
		Extensions: []string{"vector", "vchord"},
		ExtBuild:   "{}",
	}
}

func TestPlanHash(t *testing.T) {
	base := PlanHash(planIn())
	if len(base) != 64 || base != PlanHash(planIn()) {
		t.Fatalf("hash %q is not stable", base)
	}

	// Order of sets, empty versus nil, and a rewritten ext_build column do not
	// make a plan stale.
	same := planIn()
	slices.Reverse(same.Names)
	slices.Reverse(same.Recipes)
	slices.Reverse(same.Extensions)
	same.Recipes[1].Majors = []int{17, 18}
	same.ExtBuild = ""
	if got := PlanHash(same); got != base {
		t.Errorf("reordered inputs changed the hash")
	}
	empty := PlanHashIn{Names: []string{}, Pins: map[string]string{}}
	if PlanHash(empty) != PlanHash(PlanHashIn{}) {
		t.Errorf("empty and nil inputs hash differently")
	}

	// Every input moves the hash.
	for name, mut := range map[string]func(*PlanHashIn){
		"name":          func(in *PlanHashIn) { in.Names = []string{"pgvector"} },
		"recipe pkg":    func(in *PlanHashIn) { in.Recipes[1].Packages = []string{"postgresql-{major}-pgvector2"} },
		"recipe url":    func(in *PlanHashIn) { in.Recipes[0].PackageURL += "x" },
		"recipe sha":    func(in *PlanHashIn) { in.Recipes[0].SHA256["18-amd64"] = strings.Repeat("c", 64) },
		"recipe sha +":  func(in *PlanHashIn) { in.Recipes[0].SHA256["18-arm64"] = strings.Repeat("c", 64) },
		"recipe pre":    func(in *PlanHashIn) { in.Recipes[0].Preload = []string{"vchord", "x"} },
		"recipe req":    func(in *PlanHashIn) { in.Recipes[0].Requires = nil },
		"recipe ver":    func(in *PlanHashIn) { in.Recipes[0].Version = "1.1.2" },
		"pin":           func(in *PlanHashIn) { in.Pins["postgresql-17-pgvector"] = "0.8.1-1" },
		"base":          func(in *PlanHashIn) { in.Base = "postgres:17.12" },
		"arch":          func(in *PlanHashIn) { in.Arch = "arm64" },
		"node":          func(in *PlanHashIn) { in.NodeID = "n2" },
		"image":         func(in *PlanHashIn) { in.ImageRef = "x" },
		"preload":       func(in *PlanHashIn) { in.Preload = nil },
		"extensions":    func(in *PlanHashIn) { in.Extensions = []string{"vector"} },
		"ext_build":     func(in *PlanHashIn) { in.ExtBuild = `{"base":"b","ref":"r"}` },
		"ext_build bad": func(in *PlanHashIn) { in.ExtBuild = "{" },
	} {
		in := planIn()
		mut(&in)
		if PlanHash(in) == base {
			t.Errorf("changing %s kept the hash", name)
		}
	}
}

// R22: {} and malformed JSON are not recipe-built; an existing row is.
func TestExtBuildBuilt(t *testing.T) {
	for _, c := range []struct {
		in    string
		built bool
	}{
		{"", false},
		{"{}", false},
		{"{", false},
		{"null", false},
		{`{"base":"postgres:17.11"}`, false},
		{`{"ref":"r"}`, false},
		{`{"base":"","ref":"r"}`, false},
		{`{"base":"postgres:17.11","ref":"stackr/x:1"}`, true},
		{`{"base":"postgres:17.11","ref":"stackr/x:1","recipes":["pgvector"],"packages":{"postgresql-17":"17.11-1"},"arch":"amd64"}`, true},
	} {
		if got := RecipeBuilt(c.in); got != c.built {
			t.Errorf("RecipeBuilt(%q) = %v, want %v", c.in, got, c.built)
		}
		if b, err := ParseExtBuild(c.in); err == nil && b.Built() != c.built {
			t.Errorf("ParseExtBuild(%q).Built() = %v, want %v", c.in, b.Built(), c.built)
		}
	}
	if _, err := ParseExtBuild("{"); err == nil {
		t.Error("malformed JSON parsed")
	}
	b, err := ParseExtBuild(ExtBuild{Base: "b", Ref: "r", Packages: map[string]string{"p": "1"}}.String())
	if err != nil || !b.Built() || b.Packages["p"] != "1" {
		t.Errorf("String/Parse round trip: %+v, %v", b, err)
	}
	if (ExtBuild{}).String() != "{}" {
		t.Errorf("zero ExtBuild = %s", ExtBuild{}.String())
	}
}

var hostile = []string{
	"", " ", "a b", "a\tb", "a\nRUN x", "a\nFROM x", "x'y", `x"y`, "x`y", "-x", "x;y", "x$(y)", "x${y}",
	"x|y", "x&y", "x\\y", "x\ny", "../x", "X", "a\x00b",
}

func TestCheckPackage(t *testing.T) {
	for _, ok := range []string{"postgresql-{major}-pgvector", "postgresql-{major}-postgis-3-scripts", "libgdal34t64", "g++"} {
		if err := CheckPackage(ok, 17); err != nil {
			t.Errorf("CheckPackage(%q): %v", ok, err)
		}
	}
	for _, bad := range append(slices.Clone(hostile), "a", "postgresql-{major}-pg vector", "pkg=1.0", "pkg:amd64", "{minor}-x", strings.Repeat("a", 101)) {
		err := CheckPackage(bad, 17)
		if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("CheckPackage(%q) = %v, want invalid", bad, err)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, ok := range []string{"1.1.1", "17.11-1.pgdg13+2", "1:2.3~rc1", "0"} {
		if err := CheckVersion(ok); err != nil {
			t.Errorf("CheckVersion(%q): %v", ok, err)
		}
	}
	for _, bad := range append(slices.Clone(hostile), "v1", ".1", "1 2", "1;2", strings.Repeat("1", 65)) {
		if _, ok := errs.IsInvalid(CheckVersion(bad)); !ok {
			t.Errorf("CheckVersion(%q) accepted", bad)
		}
	}
}

func TestCheckPackageURL(t *testing.T) {
	good := BuiltinRecipes[2].PackageURL
	for _, ok := range []string{good, "https://example.com/a/{major}/{arch}.deb", "https://example.com/x.deb"} {
		if err := CheckPackageURL(ok); err != nil {
			t.Errorf("CheckPackageURL(%q): %v", ok, err)
		}
	}
	for _, bad := range append(slices.Clone(hostile),
		"http://example.com/x.deb", "ftp://example.com/x", "https://", "https://example.com/{minor}.deb",
		"https://example.com/{version", "https://example.com/x y.deb", "https://example.com/x'.deb",
		"https://example.com/x\".deb", "https://example.com/x\nRUN rm -rf /", "https://example.com/x\nFROM evil",
		"https://example.com/$(id)", "https://example.com/${X}", "https://example.com/;rm", "https://example.com/a`b`",
		"https://example.com/a|b", "https://example.com/a&b", " https://example.com/x", "file:///etc/passwd") {
		if _, ok := errs.IsInvalid(CheckPackageURL(bad)); !ok {
			t.Errorf("CheckPackageURL(%q) accepted", bad)
		}
	}
}

func TestCheckSHA256(t *testing.T) {
	good := strings.Repeat("0a", 32)
	if err := CheckSHA256(good); err != nil {
		t.Error(err)
	}
	for _, bad := range append(slices.Clone(hostile), good[1:], good+"0", strings.ToUpper(good), strings.Repeat("g", 64), good[:63]+"\n") {
		if _, ok := errs.IsInvalid(CheckSHA256(bad)); !ok {
			t.Errorf("CheckSHA256(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"17-amd64", "18-arm64"} {
		if err := CheckSHA256Key(ok); err != nil {
			t.Error(err)
		}
	}
	for _, bad := range append(slices.Clone(hostile), "17", "amd64", "17-", "-amd64", "17_amd64", "17-amd 64") {
		if _, ok := errs.IsInvalid(CheckSHA256Key(bad)); !ok {
			t.Errorf("CheckSHA256Key(%q) accepted", bad)
		}
	}
}

// The built-ins pass the seam's own checkers, and every vchord checksum key
// the plan names is present.
func TestBuiltinRecipesWellFormed(t *testing.T) {
	names := map[string]bool{}
	for _, r := range BuiltinRecipes {
		names[r.Name] = true
		for _, m := range r.Majors {
			for _, p := range r.Packages {
				if err := CheckPackage(p, m); err != nil {
					t.Errorf("%s: %v", r.Name, err)
				}
			}
		}
		if r.PackageURL != "" {
			if err := CheckPackageURL(r.PackageURL); err != nil {
				t.Errorf("%s: %v", r.Name, err)
			}
			if err := CheckVersion(r.Version); err != nil {
				t.Errorf("%s: %v", r.Name, err)
			}
		}
		for k, v := range r.SHA256 {
			if err := errors.Join(CheckSHA256Key(k), CheckSHA256(v)); err != nil {
				t.Errorf("%s: %v", r.Name, err)
			}
		}
		if err := CheckExtNames(r.Extensions); err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
		if err := CheckPreload(r.Preload); err != nil {
			t.Errorf("%s: %v", r.Name, err)
		}
	}
	for _, want := range []string{"pgvector", "postgis", "vchord"} {
		if !names[want] {
			t.Errorf("no built-in %s", want)
		}
	}
	v := BuiltinRecipes[2]
	for _, k := range []string{"17-amd64", "17-arm64", "18-amd64", "18-arm64"} {
		if v.SHA256[k] == "" {
			t.Errorf("vchord has no checksum for %s", k)
		}
	}
	if !slices.Equal(v.Requires, []string{"pgvector"}) || !slices.Equal(v.Preload, []string{"vchord"}) {
		t.Errorf("vchord requires %v preload %v", v.Requires, v.Preload)
	}
}
