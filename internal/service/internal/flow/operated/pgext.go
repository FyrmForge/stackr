package operated

import (
	"context"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres extensions on operated instances: the names a slice may list, the
// startup modules an instance loads, and the reconcile that makes a database
// hold what its slice lists. The checks and ExtMissing are real; reconcile,
// the reads and the boot check are stubs the engine lane fills in. The stubs
// that sit on a path every deploy takes (pgReconcile, Reconcile,
// ReconcileSlice) do nothing and return nil, so deploys behave as before.

// extName is the one pattern for an extension name and a startup module name.
var extName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// CheckExtNames refuses a name that is not an extension name, before anything
// is stored (message 9). Names reach SQL only through quote, which escapes
// nothing, so only names this accepts may go in.
func CheckExtNames(names []string) error {
	for _, n := range names {
		if !extName.MatchString(n) {
			return errs.Invalidf("extensions", "%q is not an extension name", n)
		}
	}
	return nil
}

// CheckPreload refuses a name that is not a startup module name, and
// pg_stat_statements, which is always loaded (message 10).
func CheckPreload(names []string) error {
	for _, n := range names {
		switch {
		case n == "pg_stat_statements":
			return errs.Invalidf("preload", "pg_stat_statements is always loaded; leave it out")
		case !extName.MatchString(n):
			return errs.Invalidf("preload", "%q is not a startup module name", n)
		}
	}
	return nil
}

// PGOfficial reports whether image is the stock postgres image: any tag or
// digest of postgres, library/postgres or docker.io/library/postgres. An
// empty image is the stock default, so it is official.
func PGOfficial(image string) bool {
	if image == "" {
		return true
	}
	ref, _, _ := strings.Cut(image, "@")
	repo, _, _ := strings.Cut(ref, ":")
	return pgOfficialRepo(repo)
}

// ExtMissing are the names of sliceList that instanceList does not list, in
// the slice's order, each once. A slice may only list what its instance lists.
func ExtMissing(instanceList, sliceList []string) []string {
	var out []string
	for _, n := range sliceList {
		if !slices.Contains(instanceList, n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// errExtNotBuilt is what a stub that has no honest zero answer returns.
var errExtNotBuilt = errors.New("postgres extensions: not built yet")

// ExtInfo is one extension as a drawer shows it: installed and available
// version, whether the list names it, whether it is a startup module, and
// which extension pulled it in (CASCADE).
type ExtInfo struct {
	Name, Installed, Available, AddedWith string
	Listed, Startup                       bool
}

// pgReconcile makes database db hold the extensions in want. Stub: does
// nothing.
func pgReconcile(ctx context.Context, i Instance, x Tools, db string, want []string, log io.Writer) error {
	return nil
}

// Reconcile makes every database of instance it hold what its slice lists and
// updates the extensions installed there. It runs through the primary's tools
// only, never on a standby. Stub: does nothing.
func (f *Flow) Reconcile(ctx context.Context, it store.Tile, log io.Writer) error {
	return nil
}

// ReconcileSlice is Reconcile for one slice's database, after its provision.
// Stub: does nothing.
func (f *Flow) ReconcileSlice(ctx context.Context, it, s store.Tile, log io.Writer) error {
	return nil
}

// Extensions reads instance it's extensions for its drawer. Stub: none.
func (f *Flow) Extensions(ctx context.Context, it store.Tile) ([]ExtInfo, error) {
	return nil, nil
}

// SliceExtensions reads slice s's extensions for its drawer. Stub: none.
func (f *Flow) SliceExtensions(ctx context.Context, s store.Tile) ([]ExtInfo, error) {
	return nil, nil
}

// BootCheck is the one-shot container that starts a throwaway cluster of
// Image with the startup modules and reads which extensions it offers, before
// anything is stored.
type BootCheck struct {
	Name, Image string
	Major       int
	Preload     []string
	Conf        string
}

// Spec is the container to run. Stub: the zero spec.
func (b BootCheck) Spec() docker.ContainerSpec { return docker.ContainerSpec{} }

// ParseBootCheck reads the check's output into extension name to default
// version. Stub: errors.
func ParseBootCheck(out string) (avail map[string]string, err error) {
	return nil, errExtNotBuilt
}
