package operated

import (
	"context"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Postgres extensions on operated instances: the names a slice may list, the
// startup modules an instance loads, and the reconcile that makes a database
// hold what its slice lists.

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

// ExtInfo is one extension as a drawer shows it: installed and available
// version, whether the list names it, whether it is a startup module, and
// which extension pulled it in (CASCADE).
type ExtInfo struct {
	Name, Installed, Available, AddedWith string
	Listed, Startup                       bool
}

// pgExtLockKey is the advisory lock a reconcile holds, in the database it
// works on, while it creates and updates: two reconciles of one database run
// one after the other.
const pgExtLockKey = 7370303001

// catName is a name read back from the catalog that may go in SQL.
var catName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

// pgExtRow is one extension of pg_available_extensions in one database.
type pgExtRow struct {
	installed, def string
	// by are the extensions that depend on it (CASCADE put it there).
	by []string
}

// pgReadExt reads db's extension catalog in one query, with whether the
// server is a standby.
func pgReadExt(ctx context.Context, i Instance, x Tools, db string) (recovery bool, rows map[string]pgExtRow, err error) {
	out, err := x.Exec(ctx, append(pg{}.psql(i, db, nil), "-tA", "-c",
		`SELECT 'R|' || pg_is_in_recovery()::int `+
			`UNION ALL SELECT 'E|' || a.name || '|' || coalesce(a.installed_version, '') || '|' || coalesce(a.default_version, '') || '|' || `+
			`coalesce((SELECT string_agg(e.extname, ',' ORDER BY e.extname) FROM pg_depend d `+
			`JOIN pg_extension r ON r.oid = d.refobjid JOIN pg_extension e ON e.oid = d.objid `+
			`WHERE d.classid = 'pg_extension'::regclass AND d.refclassid = 'pg_extension'::regclass `+
			`AND d.deptype = 'n' AND r.extname = a.name), '') FROM pg_available_extensions a`))
	if err != nil {
		return false, nil, err
	}
	rows = map[string]pgExtRow{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "R|1":
			recovery = true
		case strings.HasPrefix(l, "E|"):
			f := strings.SplitN(l[2:], "|", 4)
			if len(f) != 4 || !catName.MatchString(f[0]) {
				continue
			}
			r := pgExtRow{installed: f[1], def: f[2]}
			if f[3] != "" {
				r.by = strings.Split(f[3], ",")
			}
			rows[f[0]] = r
		}
	}
	return recovery, rows, nil
}

// pgReconcile makes database db hold the extensions in want and brings every
// installed extension to the image's default version. It never runs on a
// standby (it returns nil there). Nothing to do costs the one catalog read;
// otherwise one session takes an advisory lock, creates, then updates.
func pgReconcile(ctx context.Context, i Instance, x Tools, db string, want []string, log io.Writer) error {
	return pgReconcileAs(ctx, i, x, db, want, log, false)
}

// pgReconcileAs is pgReconcile; warnUpdates keeps a major change's old rule:
// an extension that cannot update is said and left, only a listed name that
// cannot be created fails.
func pgReconcileAs(ctx context.Context, i Instance, x Tools, db string, want []string, log io.Writer, warnUpdates bool) error {
	recovery, rows, err := pgReadExt(ctx, i, x, db)
	if err != nil {
		return err
	}
	if recovery {
		return nil
	}
	var create []string
	for _, n := range want {
		if !extName.MatchString(n) {
			return errs.Invalidf("extensions", "%q is not an extension name", n)
		}
		r, ok := rows[n]
		if !ok {
			return errs.Conflictf("slice %s: the image of %s has no extension %s.", db, i.Slug, n)
		}
		if r.installed == "" && !slices.Contains(create, n) {
			create = append(create, n)
		}
	}
	var update []string
	for n, r := range rows {
		if r.installed == "" || r.def == "" || r.installed == r.def {
			continue
		}
		if !catName.MatchString(n) {
			logf(log, "warning: extension %q in %s was not updated: not a plain name\n", n, db)
			continue
		}
		update = append(update, n)
	}
	sort.Strings(update)
	if len(create) == 0 && len(update) == 0 {
		return nil
	}
	lock := "SELECT pg_advisory_lock(" + strconv.Itoa(pgExtLockKey) + ")"
	if warnUpdates {
		if len(create) > 0 {
			stmts := []string{lock}
			for _, n := range create {
				stmts = append(stmts, "CREATE EXTENSION IF NOT EXISTS "+quote(n)+" CASCADE")
			}
			if err := pgExecSQL(ctx, x, i, db, stmts); err != nil {
				line, detail := psqlLine(err)
				if idx := line - 1; idx >= 1 && idx <= len(create) {
					return fmt.Errorf("extension %s was not created in %s: %s", create[idx-1], db, detail)
				}
				return fmt.Errorf("extensions in %s: %s", db, detail)
			}
		}
		for _, n := range update {
			if err := pgExecSQL(ctx, x, i, db, []string{lock, "ALTER EXTENSION " + quote(n) + " UPDATE"}); err != nil {
				_, detail := psqlLine(err)
				logf(log, "warning: extension %s in %s was not updated: %s\n", n, db, detail)
			}
		}
		return nil
	}
	stmts := []string{lock}
	for _, n := range create {
		stmts = append(stmts, "CREATE EXTENSION IF NOT EXISTS "+quote(n)+" CASCADE")
	}
	for _, n := range update {
		stmts = append(stmts, "ALTER EXTENSION "+quote(n)+" UPDATE")
	}
	err = pgExecSQL(ctx, x, i, db, stmts)
	if err == nil {
		return nil
	}
	// pgSQL writes one statement per line: psql:<stdin>:N is statement N-1
	line, detail := psqlLine(err)
	if idx := line - 1; idx >= 1 && idx < len(stmts) {
		if idx <= len(create) {
			return fmt.Errorf("extension %s was not created in %s: %s", create[idx-1], db, detail)
		}
		n := update[idx-1-len(create)]
		return fmt.Errorf("extension %s in %s was not updated to %s: %s", n, db, rows[n].def, detail)
	}
	return fmt.Errorf("extensions in %s: %s", db, detail)
}

var psqlStdin = regexp.MustCompile(`psql:<stdin>:(\d+): `)

// psqlLine is the statement line a psql error names (0 = none) and the
// error's text without that prefix.
func psqlLine(err error) (int, string) {
	msg := err.Error()
	if x, ok := docker.IsExit(err); ok {
		msg = stderrTail(x)
	}
	for _, l := range strings.Split(msg, "\n") {
		if m := psqlStdin.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, strings.TrimSpace(strings.Replace(l, m[0], "", 1))
		}
	}
	return 0, strings.TrimSpace(msg)
}

// Reconcile makes every database of instance it hold what its slice lists and
// updates the extensions installed there. It runs through the primary's tools
// only (open item 1: Exec can land on a standby, so pgReconcile itself does
// nothing in recovery). Not postgres: nil.
func (f *Flow) Reconcile(ctx context.Context, it store.Tile, log io.Writer) error {
	return f.reconcile(ctx, it, log, false)
}

func (f *Flow) reconcile(ctx context.Context, it store.Tile, log io.Writer, warnUpdates bool) error {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return err
	}
	if _, ok := e.(pg); !ok {
		return nil
	}
	i, x := f.facts(ctx, it, m, e.Definition()), f.tools(ctx, it, m)
	ps, err := f.Instances.ByInstance(ctx, m.ID)
	if err != nil {
		return err
	}
	want := map[string][]string{}
	for _, p := range ps {
		t, err := f.Tiles.Get(ctx, p.TileID)
		if err != nil {
			return err
		}
		want[p.DBName] = t.Extensions
	}
	dbs, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", pgDBsSQL))
	if err != nil {
		return err
	}
	for _, db := range strings.Fields(dbs) {
		if err := pgReconcileAs(ctx, i, x, db, want[db], log, warnUpdates); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileSlice is Reconcile for one slice's database, after its provision.
// Not provisioned or not postgres: nil.
func (f *Flow) ReconcileSlice(ctx context.Context, it, s store.Tile, log io.Writer) error {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return err
	}
	if _, ok := e.(pg); !ok {
		return nil
	}
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return err
	}
	// the caller's copy may be older than an edit
	if cur, err := f.Tiles.Get(ctx, s.ID); err == nil {
		s = cur
	}
	return pgReconcile(ctx, f.facts(ctx, it, m, e.Definition()), f.tools(ctx, it, m), p.DBName, s.Extensions, log)
}

// Extensions reads instance it's extensions for its drawer: every name it
// lists or loads at start, with the version installed in its databases (the
// first that has it) and the one the image offers, then every other
// extension the image offers, neither listed nor loaded (what a new list may
// name). Not postgres: none.
func (f *Flow) Extensions(ctx context.Context, it store.Tile) ([]ExtInfo, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	if _, ok := e.(pg); !ok {
		return nil, nil
	}
	names := slices.Clone(m.Extensions)
	for _, n := range m.Preload {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	i, x := f.facts(ctx, it, m, e.Definition()), f.tools(ctx, it, m)
	dbs, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", pgDBsSQL))
	if err != nil {
		return nil, err
	}
	out := make([]ExtInfo, len(names))
	for k, n := range names {
		out[k] = ExtInfo{Name: n, Listed: slices.Contains(m.Extensions, n), Startup: slices.Contains(m.Preload, n)}
	}
	for _, db := range strings.Fields(dbs) {
		_, rows, err := pgReadExt(ctx, i, x, db)
		if err != nil {
			return nil, err
		}
		for k := range out {
			r, ok := rows[out[k].Name]
			if !ok {
				continue
			}
			if out[k].Available == "" {
				out[k].Available = r.def
			}
			if out[k].Installed == "" {
				out[k].Installed = r.installed
			}
			if out[k].AddedWith == "" && len(r.by) > 0 {
				out[k].AddedWith = r.by[0]
			}
		}
		for _, n := range slices.Sorted(maps.Keys(rows)) {
			if !slices.Contains(names, n) && rows[n].def != "" {
				names = append(names, n)
				add := ExtInfo{Name: n, Available: rows[n].def, Installed: rows[n].installed}
				if len(rows[n].by) > 0 {
					add.AddedWith = rows[n].by[0]
				}
				out = append(out, add)
			}
		}
	}
	return out, nil
}

// SliceExtensions reads slice s's extensions for its drawer: the ones it
// lists and the ones CASCADE added for them. Not provisioned or not
// postgres: none.
func (f *Flow) SliceExtensions(ctx context.Context, s store.Tile) ([]ExtInfo, error) {
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return nil, err
	}
	m, it, e, err := f.of(ctx, p)
	if err != nil {
		return nil, err
	}
	if _, ok := e.(pg); !ok {
		return nil, nil
	}
	_, rows, err := pgReadExt(ctx, f.facts(ctx, it, m, e.Definition()), f.tools(ctx, it, m), p.DBName)
	if err != nil {
		return nil, err
	}
	var out []ExtInfo
	in := func(n string) bool { return slices.ContainsFunc(out, func(x ExtInfo) bool { return x.Name == n }) }
	add := func(n string, listed bool) {
		r := rows[n]
		x := ExtInfo{Name: n, Installed: r.installed, Available: r.def, Listed: listed}
		for _, by := range r.by {
			if x.AddedWith = by; in(by) {
				break
			}
		}
		out = append(out, x)
	}
	for _, n := range s.Extensions {
		if !in(n) {
			add(n, true)
		}
	}
	// what listed names pulled in, however deep
	for grew := true; grew; {
		grew = false
		for _, n := range slices.Sorted(maps.Keys(rows)) {
			r := rows[n]
			if r.installed == "" || in(n) || !slices.ContainsFunc(r.by, in) {
				continue
			}
			add(n, false)
			grew = true
		}
	}
	return out, nil
}

// LabelPGBootCheck marks a boot check container: its name, so a leftover is
// found and removed.
const LabelPGBootCheck = "stackr.pgbootcheck"

// pgBootScript is the boot check: POSIX sh (dash on the Debian images,
// busybox on alpine), run as postgres. It holds no input: the start flags and
// the dotted settings arrive in the environment (FLAGS, CONF). Binaries are
// found through pg_config, as the Debian and alpine layouts differ. One psql
// call, its output not piped, so its exit status is the check's.
const pgBootScript = `set -e
B=$(pg_config --bindir 2>/dev/null || true)
if [ -n "$B" ]; then PATH="$B:$PATH"; fi
D=$(mktemp -d /tmp/bc.XXXXXX)
trap 'pg_ctl -D "$D" -m immediate stop >/dev/null 2>&1 || true; rm -rf "$D"' EXIT
initdb -D "$D" -U bc --auth=trust >/dev/null
if [ -n "$CONF" ]; then printf '%s\n' "$CONF" >> "$D/postgresql.conf"; fi
pg_ctl -D "$D" -w -t 60 -o "$FLAGS -c listen_addresses= -k $D -c fsync=off" -l "$D/log" start >/dev/null || { tail -n 40 "$D/log"; exit 1; }
psql -v ON_ERROR_STOP=1 -h "$D" -U bc -d postgres -tA -c "SELECT 'X' || chr(9) || name || chr(9) || default_version FROM pg_available_extensions ORDER BY name"
`

// BootCheck is the one-shot container that starts a throwaway cluster of
// Image with the startup modules and reads which extensions it offers, before
// anything is stored.
type BootCheck struct {
	Name, Image string
	Major       int
	Preload     []string
	Conf        string
}

// Spec is the container to run. Preload and Conf (checked names and dotted
// settings) ride in its environment, never in the script.
func (b BootCheck) Spec() docker.ContainerSpec {
	var flags []string
	for _, f := range pgFlags(b.Major, 0, 0, b.Preload) {
		flags = append(flags, "-c", f)
	}
	return docker.ContainerSpec{
		Name: b.Name, Image: b.Image, User: "postgres", Restart: "no",
		Cmd:    []string{"sh", "-c", pgBootScript},
		Env:    []string{"HOME=/var/lib/postgresql", "FLAGS=" + strings.Join(flags, " "), "CONF=" + b.Conf},
		Labels: map[string]string{LabelPGBootCheck: b.Name},
	}
}

// ParseBootCheck reads the check's output into extension name to default
// version. Any other line (initdb and server noise) is ignored.
func ParseBootCheck(out string) (avail map[string]string, err error) {
	avail = map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		if len(f) == 3 && f[0] == "X" && catName.MatchString(f[1]) {
			avail[f[1]] = f[2]
		}
	}
	if len(avail) == 0 {
		return nil, fmt.Errorf("the boot check listed no extensions")
	}
	return avail, nil
}
