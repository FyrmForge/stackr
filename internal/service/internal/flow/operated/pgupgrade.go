package operated

import (
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Major version changes. The upgrade runs pg_upgrade in copy mode into a new
// volume; the downgrade dumps with the newer pg_dumpall and loads into a new
// volume. Both run in one image that holds the server binaries of every major
// stackr supports (docker/pgupgrade), and both leave the old volume as it
// was. The orchestration is service/pgupgrade.go's; this file is what runs in
// the container, what it prints, and how the new cluster is checked.

// PGUpgradeImage is the image the upgrade and the downgrade run in.
// ponytail: pin it by digest (ghcr.io/fyrmforge/stackr-pgupgrade@sha256:...)
// once docker/pgupgrade has been pushed the first time, as PoolerImage.
const PGUpgradeImage = "ghcr.io/fyrmforge/stackr-pgupgrade:1"

// LabelPGUpgrade marks a helper container of an upgrade: its instance's id.
const LabelPGUpgrade = "stackr.pgupgrade"

// The helper's steps. facts reads the old cluster (and counts its rows);
// check is pg_upgrade --check into a scratch cluster; upgrade is pg_upgrade
// itself into the new volume; dump loads the old cluster into a new one made
// by the older major.
const (
	PGStepFacts   = "facts"
	PGStepCheck   = "check"
	PGStepUpgrade = "upgrade"
	PGStepDump    = "dump"
)

// PGCountLimit is the size under which a table is counted with count(*); a
// larger one is compared by its size on disk (a copy keeps it) and said so.
const PGCountLimit int64 = 1 << 30

// PGUpgradeNeed is the free space a new volume needs: the old data again, and
// a fifth over for what the new cluster adds.
func PGUpgradeNeed(size int64) int64 { return size + size/5 }

// PGChangeCheck is what a major change may be asked to do: both majors are
// ones stackr runs and the target is not over.
func PGChangeCheck(from, to int, now time.Time) error {
	if _, ok := PGReleaseOf(to); !ok {
		return errs.Invalidf("to", "postgres %d is not one stackr runs; pick %d to %d.", to, PGReleases[0].Major, PGNewestMajor())
	}
	switch {
	case from == 0:
		return errs.Conflictf("the instance has no recorded major version")
	case to == from:
		return errs.Conflictf("it already runs postgres %d; pick another major", from)
	case PGEOL(to, now):
		return errs.Invalidf("to", "postgres %d no longer gets security fixes; pick a newer major.", to)
	}
	return nil
}

// ClusterFacts is what a new cluster must be made like for the old one to move
// in: pg_upgrade refuses a different encoding, locale or checksum setting.
type ClusterFacts struct {
	Boot, Encoding, Collate, Ctype string
	Checksums                      bool
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)

// ParseFacts reads the helper's F lines.
// ponytail: libc collations only; a cluster initialised with ICU is refused
// (initdb would need --locale-provider and the ICU locale).
func ParseFacts(out string) (ClusterFacts, error) {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if p := strings.SplitN(strings.TrimRight(l, "\r"), "\t", 3); len(p) == 3 && p[0] == "F" {
			kv[p[1]] = p[2]
		}
	}
	c := ClusterFacts{Boot: kv["boot"], Encoding: kv["enc"], Collate: kv["collate"], Ctype: kv["ctype"], Checksums: kv["sums"] == "on"}
	for _, v := range []string{c.Boot, c.Encoding, c.Collate, c.Ctype} {
		if !safeWord.MatchString(v) {
			return c, fmt.Errorf("could not read the old cluster's encoding and locale (got %q)", v)
		}
	}
	if kv["prov"] != "" && kv["prov"] != "c" {
		return c, errs.Conflictf("the database was made with ICU collations, which a major change does not carry over yet")
	}
	return c, nil
}

// InitdbArgs makes the new cluster match the old one. Checksums are on by
// default from 18 and off before: only a mismatch needs a flag.
func (c ClusterFacts) InitdbArgs(to int) []string {
	a := []string{"--encoding=" + c.Encoding, "--lc-collate=" + c.Collate, "--lc-ctype=" + c.Ctype}
	switch {
	case c.Checksums:
		a = append(a, "--data-checksums")
	case to >= 18:
		a = append(a, "--no-data-checksums")
	}
	return a
}

// The queries the helper and the new instance both ask, so their answers
// compare: the databases, and one line per user table with its row count (a
// table over the limit: its size on disk).
const (
	pgDBsSQL   = `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1`
	pgCountSQL = `SELECT CASE WHEN pg_relation_size(c.oid) <= %[1]d THEN 'C' ELSE 'E' END, current_database(), n.nspname || '.' || c.relname, ` +
		`CASE WHEN pg_relation_size(c.oid) <= %[1]d THEN (xpath('/row/c/text()', query_to_xml(format('select count(*) as c from %%I.%%I', n.nspname, c.relname), false, true, '')))[1]::text ` +
		`ELSE pg_relation_size(c.oid)::text END ` +
		`FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace ` +
		`WHERE c.relkind IN ('r', 'm') AND c.relispopulated AND n.nspname NOT IN ('pg_catalog', 'information_schema') ` +
		`AND n.nspname !~ '^pg_(toast|temp)' ORDER BY 3`
)

func countSQL(limit int64) string { return fmt.Sprintf(pgCountSQL, limit) }

// CompareCounts reports what differs between two outputs of the count query:
// a table gone, new, or with another row count. A table over the limit is
// compared by size when bySize (a copy keeps it; a dump and load does not) and
// by presence only otherwise; big says how many there were.
func CompareCounts(before, after string, bySize bool) (diff []string, big int) {
	parse := func(out string) map[string][2]string {
		m := map[string][2]string{}
		for _, l := range strings.Split(out, "\n") {
			if p := strings.Split(strings.TrimRight(l, "\r"), "\t"); len(p) == 4 && (p[0] == "C" || p[0] == "E") {
				m[p[1]+" "+p[2]] = [2]string{p[0], p[3]}
			}
		}
		return m
	}
	b, a := parse(before), parse(after)
	for k, bv := range b {
		av, ok := a[k]
		switch {
		case !ok:
			diff = append(diff, k+" is missing")
		case bv[0] == "E":
			big++
			if bySize && (av[0] != "E" || av[1] != bv[1]) {
				diff = append(diff, fmt.Sprintf("%s is %s bytes, was %s", k, av[1], bv[1]))
			}
		case av != bv:
			diff = append(diff, fmt.Sprintf("%s has %s rows, had %s", k, av[1], bv[1]))
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			diff = append(diff, k+" is new")
		}
	}
	sort.Strings(diff)
	return diff, big
}

// pgUpgradeScript is the helper's one program; STEP picks the part. It runs
// as root to lay the new volume out, and everything that touches a cluster as
// postgres. The old cluster is only ever started on a private socket. A
// check never needs the new volume: its scratch cluster lives in the
// container. A failed pg_upgrade prints the files it names its reasons in.
// ponytail: the dump of a downgrade drops one setting a newer pg_dump
// writes that older servers do not know (transaction_timeout, from 17); any
// other difference fails the load and is said.
const pgUpgradeScript = `set -eo pipefail
OLDBIN=/usr/lib/postgresql/$OLDMAJOR/bin; NEWBIN=/usr/lib/postgresql/$NEWMAJOR/bin
as() { gosu postgres "$@"; }
tab=$(printf '\t')
q() { db=$1; shift; as "$OLDBIN/psql" -X -A -t -h /tmp -U "$ADMIN" -d "$db" "$@"; }
startold() { as "$OLDBIN/pg_ctl" -D "$OLDDIR" -w -s -l /tmp/old.log -o "-c listen_addresses= -c unix_socket_directories=/tmp -c archive_mode=off" start || { cat /tmp/old.log; exit 1; }; }
stopold() { as "$OLDBIN/pg_ctl" -D "$OLDDIR" -m fast -w -s stop || true; }
mknew() {
  mkdir -p "$NEWDIR"; chown postgres:postgres "$NEWDIR"; chmod 700 "$NEWDIR"
  as "$NEWBIN/initdb" -D "$NEWDIR" -U "$ADMIN" $INITDB_FLAGS >/dev/null
  if [ "$STEP" != check ]; then
    cp "$OLDDIR/pg_hba.conf" "$OLDDIR/pg_ident.conf" "$NEWDIR/"
    printf "\nlisten_addresses = '*'\n" >> "$NEWDIR/postgresql.conf"
  fi
}
case "$STEP" in
facts)
  startold; trap stopold EXIT
  f() { printf 'F\t%s\t%s\n' "$1" "$(q postgres -c "$2")"; }
  f enc "select pg_encoding_to_char(encoding) from pg_database where datname = 'template1'"
  f collate "select datcollate from pg_database where datname = 'template1'"
  f ctype "select datctype from pg_database where datname = 'template1'"
  f prov "select coalesce(to_jsonb(d)->>'datlocprovider', 'c') from pg_database d where datname = 'template1'"
  f sums "show data_checksums"
  f boot "select rolname from pg_roles where oid = 10"
  if [ -n "$COUNTS" ]; then
    for db in $(q postgres -c "$DBS_SQL"); do q "$db" -F "$tab" -c "$COUNT_SQL"; done
  fi
  ;;
check|upgrade)
  [ "$STEP" = upgrade ] && find /new -mindepth 1 -delete
  mknew
  mkdir -p /tmp/pgu; chown postgres:postgres /tmp/pgu; cd /tmp/pgu
  if ! as "$NEWBIN/pg_upgrade" -b "$OLDBIN" -B "$NEWBIN" -d "$OLDDIR" -D "$NEWDIR" --copy -j "$(nproc)" $PGU_FLAGS; then
    find /tmp/pgu -name '*.txt' -exec cat {} + 2>/dev/null | tail -n 40
    exit 1
  fi
  ;;
dump)
  find /new -mindepth 1 -delete
  mknew
  startold; trap stopold EXIT
  as "$NEWBIN/pg_ctl" -D "$NEWDIR" -w -s -l /tmp/new.log -o "-c listen_addresses= -c unix_socket_directories=/tmp -c port=5433" start || { cat /tmp/new.log; exit 1; }
  as "$OLDBIN/pg_dumpall" -h /tmp -U "$ADMIN" | sed -e "/^CREATE ROLE $ADMIN;\$/d" -e '/^SET transaction_timeout/d' \
    | as "$NEWBIN/psql" -X -q -h /tmp -p 5433 -U "$ADMIN" -d postgres -v ON_ERROR_STOP=1 -f - >/dev/null
  as "$NEWBIN/pg_ctl" -D "$NEWDIR" -m fast -w -s stop
  ;;
esac
`

// UpgradeRun is one helper container.
type UpgradeRun struct {
	Name, Step     string
	Instance       string // the instance's id, the helper's label
	OldVol, NewVol string // Docker volume names; NewVol only for upgrade and dump
	From, To       int
	Admin          string
	Facts          ClusterFacts
	Counts         bool // facts: count the rows too
	ScratchOld     bool // OldVol is a copy with the data directory at its root
}

// Spec is the container: both volumes at fixed paths, the script, and the
// parameters in the env (no secret among them).
func (r UpgradeRun) Spec() (docker.ContainerSpec, error) {
	if !safeWord.MatchString(r.Admin) {
		return docker.ContainerSpec{}, errs.Conflictf("the admin role %q has a name a major change cannot handle", r.Admin)
	}
	oldDir, newDir := path.Join("/old", PGDataPrefix(r.From)), path.Join("/new", PGDataPrefix(r.To))
	if r.ScratchOld {
		oldDir = "/old"
	}
	flags := ""
	if r.Step == PGStepCheck {
		newDir, flags = "/tmp/new", "--check"
	}
	env := []string{
		"STEP=" + r.Step, "OLDMAJOR=" + strconv.Itoa(r.From), "NEWMAJOR=" + strconv.Itoa(r.To),
		"ADMIN=" + r.Admin, "OLDDIR=" + oldDir, "NEWDIR=" + newDir, "PGU_FLAGS=" + flags,
		"INITDB_FLAGS=" + strings.Join(r.Facts.InitdbArgs(r.To), " "),
		"DBS_SQL=" + pgDBsSQL, "COUNT_SQL=" + countSQL(PGCountLimit),
	}
	if r.Counts {
		env = append(env, "COUNTS=1")
	}
	vols := []string{r.OldVol + ":/old"}
	if r.NewVol != "" {
		vols = append(vols, r.NewVol+":/new")
	}
	return docker.ContainerSpec{
		Name: r.Name, Image: PGUpgradeImage, Cmd: []string{"bash", "-c", pgUpgradeScript},
		Env: env, Labels: map[string]string{LabelPGUpgrade: r.Instance}, Volumes: vols, Restart: "no",
	}, nil
}

// PGLogin is one role and the database it must reach on the new cluster.
type PGLogin struct{ User, Password, DB string }

// pgLoginScript logs in over the container's own address, which pg_hba
// answers with a password (the loopback lines trust): the passfile arrives on
// stdin, so no secret is on an argv.
const pgLoginScript = `umask 077; f=$(mktemp); cat > "$f"; PGPASSFILE=$f PGCONNECT_TIMEOUT=10 psql -X -q -tA -h "$(hostname -i | cut -d' ' -f1)" -U "$0" -d "$1" -c 'select 1' >/dev/null; rc=$?; rm -f "$f"; exit $rc`

// PGChecks are what the new instance must answer before it is swapped in.
// PGCounts is the count query's lines over every database; PGLogins checks
// each login the stack uses still works, so a lost password hash shows now.

func (f *Flow) PGCounts(ctx context.Context, it store.Tile) (string, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return "", err
	}
	dbs, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", pgDBsSQL))
	if err != nil {
		return "", err
	}
	var all strings.Builder
	for _, db := range strings.Fields(dbs) {
		out, err := x.Exec(ctx, append(pg{}.psql(i, db, nil), "-tA", "-F", "\t", "-c", countSQL(PGCountLimit)))
		if err != nil {
			return "", fmt.Errorf("count the rows of %s: %w", db, err)
		}
		all.WriteString(out)
		all.WriteString("\n")
	}
	return all.String(), nil
}

func (f *Flow) PGLogins(ctx context.Context, it store.Tile, logins []PGLogin) error {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	logins = append([]PGLogin{{i.AdminUser, i.AdminPassword, i.AdminDB}}, logins...)
	var bad []string
	for _, l := range logins {
		pass := strings.ReplaceAll(strings.ReplaceAll(l.Password, `\`, `\\`), `:`, `\:`)
		_, err := x.ExecIn(ctx, []string{"sh", "-c", pgLoginScript, l.User, l.DB}, "*:*:*:"+l.User+":"+pass+"\n")
		if err != nil {
			bad = append(bad, l.User+" on "+l.DB)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("these logins do not work on the new version: %s", strings.Join(bad, ", "))
	}
	return nil
}

// PGAfterChange brings the new cluster's extensions to the new version's and
// builds the statistics a copy does not carry (vacuumdb in stages, so queries
// plan well soon). An extension that cannot update is said and left.
func (f *Flow) PGAfterChange(ctx context.Context, it store.Tile, log io.Writer) error {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return err
	}
	dbs, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", pgDBsSQL))
	if err != nil {
		return err
	}
	for _, db := range strings.Fields(dbs) {
		exts, err := x.Exec(ctx, append(pg{}.psql(i, db, nil), "-tA", "-c", `SELECT extname FROM pg_extension ORDER BY 1`))
		if err != nil {
			return err
		}
		for _, e := range strings.Fields(exts) {
			if _, err := x.Exec(ctx, pg{}.psql(i, db, []string{"ALTER EXTENSION " + quote(e) + " UPDATE"})); err != nil {
				logf(log, "warning: extension %s in %s was not updated: %v\n", e, db, err)
			}
		}
	}
	_, err = x.Exec(ctx, []string{"vacuumdb", "-h", pgSocketDir, "-U", i.AdminUser, "--all", "--analyze-in-stages"})
	return err
}
