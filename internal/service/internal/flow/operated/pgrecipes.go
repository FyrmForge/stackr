package operated

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres recipes: how an extension gets into an image stackr builds on top
// of the official one. A recipe lists Debian packages (PGDG, installed under
// apt hold so the server never moves) and, where no package exists, a
// checksummed URL. The seam holds the types, the built-ins, the ExtBuild
// record and the input checkers; lane A2 fills in the rest.

// errRecipeNotBuilt is what a stub of the recipe engine returns until lane A2
// builds its body.
var errRecipeNotBuilt = errors.New("postgres recipes are not built yet")

// Recipe is one extension recipe. Packages and PackageURL may carry {major}
// (and PackageURL also {version} and {arch}); SHA256 is keyed "<major>-<arch>"
// and holds one checksum per URL package build. Extensions are the names
// CREATE EXTENSION creates: the boot check creates every one of them in a real
// initdb, apt success alone proves nothing (Stage 0 spike 6). Requires are
// recipe names that must be enabled with it. Preload are the startup modules
// it needs.
type Recipe struct {
	Name       string            `json:"name"`
	Majors     []int             `json:"majors,omitempty"`
	Packages   []string          `json:"packages,omitempty"`
	PackageURL string            `json:"package_url,omitempty"`
	SHA256     map[string]string `json:"sha256,omitempty"`
	Version    string            `json:"version,omitempty"`
	Extensions []string          `json:"extensions,omitempty"`
	Preload    []string          `json:"preload,omitempty"`
	Requires   []string          `json:"requires,omitempty"`
}

// BuiltinRecipes are the recipes stackr ships, by name. A server file or the
// admin page can switch one off (a bare pg_recipes row with enabled=0) and add
// others; a built-in is never edited in place.
var BuiltinRecipes = []Recipe{
	{
		Name:       "pgvector",
		Majors:     []int{14, 15, 16, 17, 18},
		Packages:   []string{"postgresql-{major}-pgvector"},
		Extensions: []string{"vector"},
	},
	{
		// postgis needs its -scripts package for CREATE EXTENSION; the
		// newest postgis build may be built for a newer server than the
		// held one, so the resolver walks the madison list down and the
		// boot check creates every extension (Stage 0 spike 6).
		Name:       "postgis",
		Majors:     []int{14, 15, 16, 17, 18},
		Packages:   []string{"postgresql-{major}-postgis-3", "postgresql-{major}-postgis-3-scripts"},
		Extensions: []string{"postgis", "postgis_topology", "postgis_raster"},
	},
	{
		// vchord has no PGDG package: its release .debs are installed by
		// checksummed URL (ADD --checksum, then apt-get install of the
		// file). Debian images only (glibc). The .deb has no apt dependency
		// on pgvector (vchord.control requires vector), so Requires stays.
		// The live spike passed on postgres 17.11, 17.5, 18.7 and 18.0. The
		// sha256 values are those of the four 1.1.1-1 release .debs, checked
		// against a download of each.
		Name:       "vchord",
		Majors:     []int{17, 18},
		PackageURL: "https://github.com/supervc-stack/VectorChord/releases/download/{version}/postgresql-{major}-vchord_{version}-1_{arch}.deb",
		SHA256: map[string]string{
			"17-amd64": "a48132c8d616c0d52a9811b32f12f88a8f6cc8542d7cc0180c8d0986bfbd03c5",
			"17-arm64": "50ae776bb2376f2b21aea646984cef51e96cc8ae49626cceccb043432c3ba1aa",
			"18-amd64": "7637a18a97157db8d904bb7093e5ce33b6a395c0acac9ae6b660032f3d57a33e",
			"18-arm64": "59dbe75a398c2df2849631de054d80e60a6c04499fa2d29eebdb04e40f28501c",
		},
		Version:    "1.1.1",
		Extensions: []string{"vchord"},
		Preload:    []string{"vchord"},
		Requires:   []string{"pgvector"},
	},
}

// ExtBuild is a recipe-built instance's build record, the JSON in
// operated_instances.ext_build: the official image it was built on, the
// recipes it holds, every package at its installed version (the server and
// client packages too, Stage 3 pins them), the CPU type, the built image's
// ref, and the ref and time of the build it replaced.
type ExtBuild struct {
	Base     string            `json:"base,omitempty"`
	Recipes  []string          `json:"recipes,omitempty"`
	Packages map[string]string `json:"packages,omitempty"`
	Arch     string            `json:"arch,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	PrevRef  string            `json:"prev_ref,omitempty"`
	PrevAt   time.Time         `json:"prev_at,omitzero"`
}

// ParseExtBuild reads an ext_build column. "" and "{}" are the zero value;
// malformed JSON is an error and the zero value (which is not recipe-built).
func ParseExtBuild(s string) (ExtBuild, error) {
	var b ExtBuild
	if strings.TrimSpace(s) == "" {
		return b, nil
	}
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		return ExtBuild{}, fmt.Errorf("ext_build: %w", err)
	}
	return b, nil
}

// Built says whether the instance is recipe-built (R22): a base and a ref.
// An empty record, {} and a record that does not parse are not recipe-built.
func (b ExtBuild) Built() bool { return b.Base != "" && b.Ref != "" }

// RecipeBuilt reports whether an ext_build column holds a recipe-built
// instance. Never a string non-empty test.
func RecipeBuilt(extBuild string) bool {
	b, err := ParseExtBuild(extBuild)
	return err == nil && b.Built()
}

// String is the column text; the zero value is "{}".
func (b ExtBuild) String() string {
	out, err := json.Marshal(b)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// The input patterns of R20. Every value that reaches shell or Dockerfile
// text has passed one of these first.
var (
	pkgName    = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,99}$`)
	pkgVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~:-]{0,63}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sha256Key  = regexp.MustCompile(`^[0-9]{1,2}-[a-z0-9]{3,10}$`)
	// urlRest is what may follow https:// once the placeholders are out: no
	// whitespace, quotes, backticks, $, ;, backslash or any shell or
	// Dockerfile metacharacter. Stricter than the plan's list on purpose
	// (an allowlist), loosen it when a real recipe needs a query string.
	urlRest = regexp.MustCompile(`^[A-Za-z0-9._~:/%+@=-]+$`)
	urlHole = strings.NewReplacer("{version}", "0", "{major}", "0", "{arch}", "a")
)

// CheckPackage refuses a Debian package name, after {major} is expanded.
func CheckPackage(name string, major int) error {
	n := strings.ReplaceAll(name, "{major}", fmt.Sprint(major))
	if !pkgName.MatchString(n) {
		return errs.Invalidf("packages", "%q is not a package name", name)
	}
	return nil
}

// CheckVersion refuses a package version.
func CheckVersion(v string) error {
	if !pkgVersion.MatchString(v) {
		return errs.Invalidf("version", "%q is not a version", v)
	}
	return nil
}

// CheckPackageURL refuses a package URL: https only, only the placeholders
// {version}, {major} and {arch}, no whitespace and no quotes.
func CheckPackageURL(u string) error {
	rest, ok := strings.CutPrefix(u, "https://")
	if !ok || !urlRest.MatchString(urlHole.Replace(rest)) {
		return errs.Invalidf("package_url", "%q is not an https URL with only {version}, {major} and {arch}", u)
	}
	return nil
}

// CheckSHA256 refuses a checksum that is not exactly 64 lowercase hex.
func CheckSHA256(s string) error {
	if !sha256Hex.MatchString(s) {
		return errs.Invalidf("sha256", "%q is not a sha256 checksum", s)
	}
	return nil
}

// CheckSHA256Key refuses a checksum key that is not "<major>-<arch>".
func CheckSHA256Key(k string) error {
	if !sha256Key.MatchString(k) {
		return errs.Invalidf("sha256", "%q is not a <major>-<arch> key", k)
	}
	return nil
}

// The rest of the recipe engine is lane A2's. These stubs only fix the names;
// A2 owns this file and may change a signature.

// CheckRecipe refuses a recipe that is not well formed (message 23).
func CheckRecipe(Recipe) error { return errRecipeNotBuilt }

// ResolveRecipes returns the recipes names ask for, with their Requires
// closure, from the built-ins and the pg_recipes rows, for one major and org
// (messages 15 to 17).
func ResolveRecipes(names []string, major int, orgID string, rows []store.PGRecipe) ([]Recipe, error) {
	return nil, errRecipeNotBuilt
}

// DerivedPreload is the startup modules a set of recipes needs.
func DerivedPreload(rs []Recipe) []string { return nil }

// ResolveScript is the shell that runs in a base image and prints the newest
// version of each package that installs under hold, or the Postgres minor it
// needs; ParseResolve reads its output.
func ResolveScript(major int, pkgs []string) string { return "" }

// Resolved is a ParseResolve result: the version found for each package, or
// the Postgres minor a package needs when none installs.
type Resolved struct {
	Versions  map[string]string
	NeedMinor string
}

// ParseResolve reads ResolveScript's output.
func ParseResolve(out string) (Resolved, error) { return Resolved{}, errRecipeNotBuilt }

// RecipeDockerfile is the Dockerfile that builds the recipes on base:
// apt-mark hold, pkg=version installs, ADD --checksum for URL packages, a
// final postgres --version test and LABEL stackr.image=pgext.
func RecipeDockerfile(base string, major int, arch string, rs []Recipe, pinned map[string]string) (string, error) {
	return "", errRecipeNotBuilt
}

// BuildTag names a built image: <major>.<minor>-ext-<arch>-<8 hex> (R25).
func BuildTag(base, arch, dockerfile string) string { return "" }
