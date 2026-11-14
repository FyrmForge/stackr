package operated

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
	// URLs are the release files the build installed, by package name: the
	// helper of a later major change fetches these, not whatever the recipe
	// says by then. Older records have none (the recipe is used).
	URLs    map[string]URLPkg `json:"urls,omitempty"`
	Arch    string            `json:"arch,omitempty"`
	Ref     string            `json:"ref,omitempty"`
	PrevRef string            `json:"prev_ref,omitempty"`
	PrevAt  time.Time         `json:"prev_at,omitzero"`
}

// URLPkg is one release file a build installed.
type URLPkg struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
}

// recipeURL is the release file of recipe r for major and arch, with its
// package name (the file is named <package>_<version>_<arch>.deb).
func recipeURL(r Recipe, major int, arch string) (pkg string, u URLPkg, err error) {
	ms := strconv.Itoa(major)
	sum, ok := r.SHA256[ms+"-"+arch]
	if !ok || (len(r.Majors) > 0 && !slices.Contains(r.Majors, major)) {
		return "", URLPkg{}, errs.Refusedf("the recipe %s has no build for postgres %d on %s.", r.Name, major, arch)
	}
	url := strings.NewReplacer("{version}", r.Version, "{major}", ms, "{arch}", arch).Replace(r.PackageURL)
	if err := CheckPackageURL(url); err != nil {
		return "", URLPkg{}, err
	}
	pkg, _, _ = strings.Cut(url[strings.LastIndex(url, "/")+1:], "_")
	return pkg, URLPkg{URL: url, SHA256: sum, Version: r.Version}, nil
}

// URLPackages are the release files rs install for major and arch, by
// package name, for the build record (ExtBuild.URLs). Nil when there are none.
func URLPackages(rs []Recipe, major int, arch string) (map[string]URLPkg, error) {
	var out map[string]URLPkg
	for _, r := range rs {
		if r.PackageURL == "" {
			continue
		}
		pkg, u, err := recipeURL(r, major, arch)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]URLPkg{}
		}
		out[pkg] = u
	}
	return out, nil
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

var (
	recipeName = extName
	// baseRef is an image reference as it reaches FROM: no whitespace, no
	// quotes, no leading dash.
	baseRef  = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._/:@-]{0,199}$`)
	archName = regexp.MustCompile(`^[a-z0-9]{3,10}$`)
	// recipeArches are the CPU types a URL recipe must carry a checksum for.
	recipeArches = []string{"amd64", "arm64"}
)

func recipeMajors(r Recipe) []int {
	if len(r.Majors) == 0 {
		return []int{17}
	}
	return r.Majors
}

// CheckRecipe refuses a recipe that is not well formed (message 23). Whether
// its Requires names exist is ResolveRecipes' check.
func CheckRecipe(r Recipe) error {
	if !recipeName.MatchString(r.Name) {
		return errs.Invalidf("name", "%q is not a recipe name", r.Name)
	}
	switch {
	case len(r.Packages) > 0 && r.PackageURL != "":
		return errs.Invalidf("packages", "set packages or package_url, not both")
	case len(r.Packages) == 0 && r.PackageURL == "":
		return errs.Invalidf("packages", "set packages or package_url")
	case len(r.Extensions) == 0:
		return errs.Invalidf("extensions", "a recipe lists the extensions it creates")
	}
	for _, m := range r.Majors {
		if m < 1 || m > 99 {
			return errs.Invalidf("majors", "%d is not a postgres major", m)
		}
	}
	for _, m := range recipeMajors(r) {
		for _, p := range r.Packages {
			if err := CheckPackage(p, m); err != nil {
				return err
			}
		}
	}
	if r.PackageURL != "" {
		if err := CheckPackageURL(r.PackageURL); err != nil {
			return errs.Invalidf("package_url", "package_url must be https and may only use {version}, {major} and {arch}")
		}
		if err := CheckVersion(r.Version); err != nil {
			return err
		}
		need := errs.Invalidf("sha256", "package_url needs a version and a sha256 for every major and CPU type, keyed like 17-amd64")
		if len(r.Majors) == 0 {
			return need
		}
		for _, m := range r.Majors {
			for _, a := range recipeArches {
				if _, ok := r.SHA256[fmt.Sprintf("%d-%s", m, a)]; !ok {
					return need
				}
			}
		}
	} else if r.Version != "" {
		if err := CheckVersion(r.Version); err != nil {
			return err
		}
	}
	for k, v := range r.SHA256 {
		if err := CheckSHA256Key(k); err != nil {
			return err
		}
		if err := CheckSHA256(v); err != nil {
			return errs.Invalidf("sha256", "sha256 must be 64 hex characters")
		}
	}
	for field, names := range map[string][]string{"extensions": r.Extensions, "preload": r.Preload, "requires": r.Requires} {
		for _, n := range names {
			if !extName.MatchString(n) {
				return errs.Invalidf(field, "%q is not a valid name", n)
			}
		}
	}
	return nil
}

// recipeSpec reads a pg_recipes row that carries a recipe. The row id is the
// recipe name.
func recipeSpec(row store.PGRecipe) (Recipe, error) {
	var r Recipe
	if err := json.Unmarshal([]byte(row.Spec), &r); err != nil {
		return Recipe{}, fmt.Errorf("recipe %s: %w", row.ID, err)
	}
	r.Name = row.ID
	return r, CheckRecipe(r)
}

// candidate is one recipe a name may mean, with the verdict on using it.
type candidate struct {
	r   Recipe
	err error
}

// recipeCandidates are the recipes in effect for one org: the built-ins (a
// row switches one off or narrows its share; a row never edits a built-in) and
// the admin rows that parse, built-ins first.
func recipeCandidates(orgID string, rows []store.PGRecipe) []candidate {
	byID := map[string]store.PGRecipe{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	var out []candidate
	for _, b := range BuiltinRecipes {
		c := candidate{r: b}
		if row, ok := byID[b.Name]; ok { // no row: on and shared with all
			switch {
			case !row.Enabled:
				c.err = errs.Refusedf("the recipe %s is switched off on this server.", b.Name)
			// an empty share is the built-in default (all); only a list narrows it
			case len(row.Share) > 0 && !slices.Contains(row.Share, "all") && !slices.Contains(row.Share, orgID):
				c.err = errs.Refusedf("the recipe %s is not shared with %s.", b.Name, orgID)
			}
		}
		out = append(out, c)
	}
	for _, row := range rows {
		if slices.ContainsFunc(BuiltinRecipes, func(b Recipe) bool { return b.Name == row.ID }) || strings.TrimSpace(row.Spec) == "" {
			continue
		}
		r, err := recipeSpec(row)
		if err != nil {
			continue // stored rows were checked; one that is not cannot match
		}
		c := candidate{r: r}
		switch {
		case !row.Enabled:
			c.err = errs.Refusedf("the recipe %s is switched off on this server.", r.Name)
		case !slices.Contains(row.Share, "all") && !slices.Contains(row.Share, orgID):
			c.err = errs.Refusedf("the recipe %s is not shared with %s.", r.Name, orgID)
		}
		out = append(out, c)
	}
	return out
}

// ResolveRecipes returns the recipes names ask for, with their Requires
// closure, from the built-ins and the pg_recipes rows, for one major and org
// (messages 15 to 17). A name is an extension name (Recipe.Extensions) or a
// recipe name. The result is ordered requirements first, each recipe once.
func ResolveRecipes(names []string, major int, orgID string, rows []store.PGRecipe) ([]Recipe, error) {
	cands := recipeCandidates(orgID, rows)
	var out []Recipe
	seen := map[string]bool{}
	var add func(c candidate) error
	add = func(c candidate) error {
		if seen[c.r.Name] {
			return nil
		}
		seen[c.r.Name] = true
		if c.err != nil {
			return c.err
		}
		if (len(c.r.Majors) > 0 && !slices.Contains(c.r.Majors, major)) ||
			(c.r.PackageURL != "" && !hasShaFor(c.r, major)) {
			return errs.Refusedf("the recipe %s has no build for postgres %d.", c.r.Name, major)
		}
		for _, req := range c.r.Requires {
			i := slices.IndexFunc(cands, func(x candidate) bool { return x.r.Name == req })
			if i < 0 {
				return errs.Invalidf("requires", "requires: no recipe %s", req)
			}
			if err := add(cands[i]); err != nil {
				return err
			}
		}
		out = append(out, c.r)
		return nil
	}
	for _, n := range names {
		first, pick := -1, -1
		for i, c := range cands {
			if c.r.Name != n && !slices.Contains(c.r.Extensions, n) {
				continue
			}
			if first < 0 {
				first = i
			}
			if c.err == nil {
				pick = i
				break
			}
		}
		switch {
		case first < 0:
			return nil, errs.Refusedf("%s is not in the image and no recipe has that name.", n)
		case pick < 0:
			return nil, cands[first].err
		}
		if err := add(cands[pick]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func hasShaFor(r Recipe, major int) bool {
	for _, a := range recipeArches {
		if _, ok := r.SHA256[fmt.Sprintf("%d-%s", major, a)]; ok {
			return true
		}
	}
	return false
}

// DerivedPreload is the startup modules a set of recipes needs, in recipe
// order, each once. pg_stat_statements is not in it; it is always loaded.
func DerivedPreload(rs []Recipe) []string {
	var out []string
	for _, r := range rs {
		for _, p := range r.Preload {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// RecipePackages are the expanded apt package names of rs for a major, each
// once, in recipe order. URL recipes contribute none.
func RecipePackages(rs []Recipe, major int) []string {
	var out []string
	for _, r := range rs {
		for _, p := range r.Packages {
			p = strings.ReplaceAll(p, "{major}", strconv.Itoa(major))
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// maxSims caps the apt simulations per package. The walk-down after a failed
// boot check is pgWalkScript's, not the resolve's.
const maxSims = 30

// ResolveScript is the shell (run with sh) that goes in a base image and
// prints, per package, one line: "ok <pkg> <v>" with the newest installable
// build (under hold on the server and client packages, each tried with
// apt-get install -s), or "none <pkg>
// [minor]" with the Postgres version the newest build depends on. A failure
// prints "err <text>". pkgs are expanded names; an invalid one yields a script
// that prints err. ParseResolve reads the output.
func ResolveScript(major int, pkgs []string) string {
	for _, p := range pkgs {
		if CheckPackage(p, major) != nil {
			return "echo 'err invalid package name'; exit 1\n"
		}
	}
	if len(pkgs) == 0 || major < 1 || major > 99 {
		return "echo 'err no packages'; exit 1\n"
	}
	r := strings.NewReplacer("@MAJOR@", strconv.Itoa(major), "@PKGS@", strings.Join(pkgs, " "),
		"@SIMS@", strconv.Itoa(maxSims))
	return r.Replace(`set -u
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1 || { echo "err apt-get update failed"; exit 1; }
apt-mark hold postgresql-@MAJOR@ postgresql-client-@MAJOR@ >/dev/null 2>&1 || { echo "err apt-mark hold failed"; exit 1; }
for p in @PKGS@; do
  vs=; t=0
  for v in $(apt-cache madison "$p" | awk -F'|' '{gsub(/[ \t]/,"",$2); if ($2!="" && !s[$2]++) print $2}'); do
    t=$((t+1)); [ "$t" -gt @SIMS@ ] && break
    if apt-get install -s --no-install-recommends "$p=$v" >/dev/null 2>&1; then vs=" $v"; break; fi
  done
  if [ -n "$vs" ]; then echo "ok $p$vs"; else
    m=$(apt-get install -s --no-install-recommends "$p" 2>&1 | sed -n 's/.*Depends: postgresql-@MAJOR@ (>*=* *\([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -n1)
    echo "none $p $m"
  fi
done
`)
}

// Resolved is a ParseResolve result. Versions holds the newest installable
// build of each package; None lists the packages with no installable build and
// NeedMinor the Postgres version the first of them depends on (empty when apt
// did not say), which the caller weighs against a newer base (R26).
type Resolved struct {
	Versions  map[string]string
	None      []string
	NeedMinor string
}

var minorRE = regexp.MustCompile(`^[0-9]{1,2}\.[0-9]{1,3}$`)

// ParseResolve reads ResolveScript's output. Every version in it is checked
// with CheckVersion (it goes back into a Dockerfile).
func ParseResolve(out string) (Resolved, error) {
	res := Resolved{Versions: map[string]string{}}
	lines := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		lines++
		switch {
		case f[0] == "err":
			return Resolved{}, fmt.Errorf("resolve: %s", strings.Join(f[1:], " "))
		case f[0] == "ok" && len(f) >= 3:
			if !pkgName.MatchString(f[1]) {
				return Resolved{}, fmt.Errorf("resolve: bad package %q", f[1])
			}
			for _, v := range f[2:] {
				if err := CheckVersion(v); err != nil {
					return Resolved{}, err
				}
			}
			res.Versions[f[1]] = f[2]
		case f[0] == "none" && (len(f) == 2 || len(f) == 3):
			if !pkgName.MatchString(f[1]) {
				return Resolved{}, fmt.Errorf("resolve: bad package %q", f[1])
			}
			res.None = append(res.None, f[1])
			if len(f) == 3 && res.NeedMinor == "" && minorRE.MatchString(f[2]) {
				res.NeedMinor = f[2]
			}
		default:
			return Resolved{}, fmt.Errorf("resolve: unreadable line %q", line)
		}
	}
	if lines == 0 {
		return Resolved{}, fmt.Errorf("resolve: no output")
	}
	return res, nil
}

// RecipeDockerfile is the Dockerfile that builds the recipes on base: the
// server and client held, a pin for every apt package (a missing pin is an
// error; nothing floats), ADD --checksum for URL packages then apt-get
// install of the file, a final postgres --version and LABEL stackr.image=pgext.
// Every value written passes its R20 pattern here, whatever the caller did.
func RecipeDockerfile(base string, major int, arch string, rs []Recipe, pinned map[string]string) (string, error) {
	if !baseRef.MatchString(base) {
		return "", errs.Invalidf("base", "%q is not an image reference", base)
	}
	if !archName.MatchString(arch) {
		return "", errs.Invalidf("arch", "%q is not a CPU type", arch)
	}
	if major < 1 || major > 99 {
		return "", errs.Invalidf("major", "%d is not a postgres major", major)
	}
	// the same recipes and pins give the same file, whatever order they were
	// resolved in: a rebuild must land on the tag the build had
	rs = slices.SortedFunc(slices.Values(rs), func(a, b Recipe) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\nLABEL stackr.image=pgext\n", base)
	var debs []string
	for i, r := range rs {
		if err := CheckRecipe(r); err != nil {
			return "", err
		}
		if len(r.Majors) > 0 && !slices.Contains(r.Majors, major) {
			return "", errs.Refusedf("the recipe %s has no build for postgres %d.", r.Name, major)
		}
		if r.PackageURL == "" {
			continue
		}
		sum, ok := r.SHA256[fmt.Sprintf("%d-%s", major, arch)]
		if !ok {
			return "", errs.Refusedf("the recipe %s has no build for postgres %d on %s.", r.Name, major, arch)
		}
		url := strings.NewReplacer("{version}", r.Version, "{major}", strconv.Itoa(major), "{arch}", arch).Replace(r.PackageURL)
		if err := CheckPackageURL(url); err != nil {
			return "", err
		}
		deb := fmt.Sprintf("/tmp/ext-%d.deb", i)
		debs = append(debs, deb)
		fmt.Fprintf(&b, "ADD --checksum=sha256:%s %s %s\n", sum, url, deb)
	}
	var pins []string
	pkgs := RecipePackages(rs, major)
	sort.Strings(pkgs)
	for _, p := range pkgs {
		v, ok := pinned[p]
		if !ok {
			return "", errs.Invalidf("packages", "no pinned version for %s", p)
		}
		if err := CheckVersion(v); err != nil {
			return "", err
		}
		pins = append(pins, p+"="+v)
	}
	m := strconv.Itoa(major)
	run := "RUN apt-mark hold postgresql-" + m + " postgresql-client-" + m + " && apt-get update"
	if len(pins) > 0 {
		run += " && apt-get install -y --no-install-recommends " + strings.Join(pins, " ")
	}
	if len(debs) > 0 {
		run += " && apt-get install -y --no-install-recommends " + strings.Join(debs, " ") + " && rm -f " + strings.Join(debs, " ")
	}
	run += " && rm -rf /var/lib/apt/lists/* && postgres --version\n"
	b.WriteString(run)
	return b.String(), nil
}

var imageTagMinor = regexp.MustCompile(`^([0-9]+)\.([0-9]+)`)

// BuildTag names a built image: <major>.<minor>-ext-<arch>-<8 hex> (R25). The
// hex is the start of the Dockerfile's sha256; major.minor come from the tag
// of base; without a minor (postgres:17, a digest) it is <major>-ext-... from
// major, so ImageMajor still reads it ("unknown" only when major is 0 too).
func BuildTag(base string, major int, arch, dockerfile string) string {
	tag := base[strings.LastIndex(base, "/")+1:]
	mm := "unknown"
	if major > 0 {
		mm = strconv.Itoa(major)
	}
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		if m := imageTagMinor.FindStringSubmatch(tag[i+1:]); m != nil {
			mm = m[1] + "." + m[2]
		}
	}
	arch = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, arch)
	sum := sha256.Sum256([]byte(dockerfile))
	return mm + "-ext-" + arch + "-" + hex.EncodeToString(sum[:])[:8]
}

// HelperDockerfile is the major-change helper for a recipe-built instance:
// PGUpgradeImage with both majors' server, client and extension packages at
// the versions the two builds recorded, held, checked against the pins.
func HelperDockerfile(from, to ExtBuild, arch string, extra ...Recipe) (string, error) {
	if !archName.MatchString(arch) {
		return "", errs.Invalidf("arch", "%q is not a CPU type", arch)
	}
	all := map[string]string{}
	var servers, apt, adds, debs []string
	hold := []string{"postgresql-common", "postgresql-client-common"}
	for i, b := range []ExtBuild{from, to} {
		m, err := buildMajor(b)
		if err != nil {
			return "", err
		}
		ms := strconv.Itoa(m)
		if _, ok := b.Packages["postgresql-client-"+ms]; !ok {
			return "", errs.Refusedf("the build has no pinned postgresql-client-%s.", ms)
		}
		// the build's own record of its release files wins: a recipe edited or
		// removed since does not matter; older records ask the recipes
		urls := b.URLs
		if len(urls) == 0 {
			if urls, err = recipeURLs(b, m, arch, extra); err != nil {
				return "", err
			}
		}
		urlPkgs := map[string]bool{}
		for _, pkg := range slices.Sorted(maps.Keys(urls)) {
			u := urls[pkg]
			if err := CheckPackageURL(u.URL); err != nil {
				return "", err
			}
			if err := CheckSHA256(u.SHA256); err != nil {
				return "", err
			}
			if err := CheckPackage(pkg, m); err != nil {
				return "", err
			}
			if _, ok := b.Packages[pkg]; !ok {
				return "", errs.Refusedf("the build has no recorded version of %s.", pkg)
			}
			urlPkgs[pkg] = true
			deb := fmt.Sprintf("/tmp/ext-%s-%d.deb", ms, len(debs))
			adds = append(adds, fmt.Sprintf("ADD --checksum=sha256:%s %s %s\n", u.SHA256, u.URL, deb))
			debs = append(debs, deb)
		}
		for _, name := range slices.Sorted(maps.Keys(b.Packages)) {
			v := b.Packages[name]
			if i == 0 && (name == "postgresql-common" || name == "postgresql-client-common") {
				continue // shared by both majors: the new major's image's build wins
			}
			if err := CheckPackage(name, m); err != nil {
				return "", err
			}
			if err := CheckVersion(v); err != nil {
				return "", err
			}
			old, seen := all[name]
			if seen && old != v {
				return "", errs.Refusedf("the two builds pin %s at %s and %s.", name, old, v)
			}
			all[name] = v
			switch {
			case urlPkgs[name] || seen:
			case name == "postgresql-"+ms || name == "postgresql-client-"+ms:
				servers = append(servers, name+"="+v)
				hold = append(hold, name)
			case name == "postgresql-common" || name == "postgresql-client-common":
				servers = append(servers, name+"="+v)
			default:
				apt = append(apt, name+"="+v)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", PGUpgradeImage)
	// The server and client pins go in first (downgrades allowed: the base
	// holds the newest minors) and are held, then the rest of the base gets
	// Debian's fixes, then the extension packages.
	b.WriteString("RUN apt-get update \\\n && apt-get install -y --no-install-recommends --allow-downgrades " + strings.Join(servers, " ") + " \\\n")
	b.WriteString(" && apt-mark hold " + strings.Join(hold, " ") + " \\\n")
	b.WriteString(" && apt-get upgrade -y --no-install-recommends \\\n")
	if len(apt) > 0 {
		b.WriteString(" && apt-get install -y --no-install-recommends " + strings.Join(apt, " ") + " \\\n")
	}
	b.WriteString(" && rm -rf /var/lib/apt/lists/* /usr/local/bin/gosu\n")
	if len(debs) > 0 {
		b.WriteString(strings.Join(adds, ""))
		b.WriteString("RUN apt-get update && apt-get install -y " + strings.Join(debs, " ") + " && rm -rf /tmp/*.deb /var/lib/apt/lists/*\n")
	}
	var pins []string
	for _, n := range slices.Sorted(maps.Keys(all)) {
		pins = append(pins, n+"="+all[n])
	}
	b.WriteString(`RUN for p in ` + strings.Join(pins, " ") + `; do [ "$(dpkg-query -W -f='${Version}' "${p%%=*}")" = "${p#*=}" ] || { echo "version differs from its pin: $p" >&2; exit 1; }; done` + "\n")
	return b.String(), nil
}

// recipeURLs is a build record's release files as its recipes say them, for
// records older than ExtBuild.URLs.
func recipeURLs(b ExtBuild, m int, arch string, extra []Recipe) (map[string]URLPkg, error) {
	out := map[string]URLPkg{}
	for _, name := range b.Recipes {
		i := slices.IndexFunc(slices.Concat(extra, BuiltinRecipes), func(c Recipe) bool { return c.Name == name })
		if i < 0 {
			return nil, errs.Refusedf("the recipe %s is not known, so the helper cannot be built.", name)
		}
		r := slices.Concat(extra, BuiltinRecipes)[i]
		if err := CheckRecipe(r); err != nil {
			return nil, err
		}
		if r.PackageURL == "" {
			continue
		}
		pkg, u, err := recipeURL(r, m, arch)
		if err != nil {
			return nil, err
		}
		out[pkg] = u
	}
	return out, nil
}

var serverPkgRE = regexp.MustCompile(`^postgresql-([0-9]{1,2})$`)

// buildMajor is the one postgres major a build record holds, read from its
// pinned server package.
func buildMajor(b ExtBuild) (int, error) {
	major := 0
	for name := range b.Packages {
		if m := serverPkgRE.FindStringSubmatch(name); m != nil {
			if major != 0 {
				return 0, errs.Refusedf("the build records more than one postgres server.")
			}
			major, _ = strconv.Atoi(m[1])
		}
	}
	if major == 0 {
		return 0, errs.Refusedf("the build has no pinned postgres server.")
	}
	return major, nil
}
