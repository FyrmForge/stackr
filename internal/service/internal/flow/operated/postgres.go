package operated

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type pg struct{}

// pgSocketDir is the unix socket directory every admin client names, so a
// changed default cannot send it elsewhere; unix_socket_directories is owned.
const pgSocketDir = "/var/run/postgresql"

// ArchiveDirOf is the directory the archive command writes to and the shipper
// reads: the archive volume's root for the first epoch, e<N> under it after a
// major change or its rollback.
func ArchiveDirOf(epoch int) string {
	if epoch <= 0 {
		return PGArchiveDir
	}
	return PGArchiveDir + "/e" + strconv.Itoa(epoch)
}

// PGArchiveDir is where the archive volume mounts: finished WAL segments
// land here until the shipper takes them. Outside /var/lib/postgresql, which
// is the data volume's own mount from 18.
const PGArchiveDir = "/var/lib/stackr-wal"

// pgFlags are the settings stackr owns, as -c start flags: they beat every
// file, so neither stackr.conf nor a user can move them. allow_alter_system
// exists from 17 only. The reloadable owned settings (primary_conninfo, the
// slot cap) are in stackr-owned.conf instead (pgOwnedConf). restore_command,
// hba_file, ssl* and synchronous_standby_names are reserved too (PGOwned) but
// not emitted: a restore writes its own restore_command into
// postgresql.auto.conf, and the rest are for later phases. archive_command writes a dotfile and renames
// it, so the shipper never sees a half-copied segment, and refuses to
// overwrite one.
func pgFlags(major, pitrDays, epoch int) []string {
	// the replication settings are the same on every member: a standby needs
	// each of its limits at least the primary's
	f := []string{
		"shared_preload_libraries=pg_stat_statements", "wal_log_hints=on",
		"wal_level=replica", "max_wal_senders=10", "max_replication_slots=10", "hot_standby=on",
	}
	if major == 0 || major >= 17 {
		f = append(f, "allow_alter_system=off")
	}
	// slots kept back for stackr's admin role and replication, so a full
	// pool never locks stackr out
	if major == 0 || major >= 16 {
		f = append(f, "reserved_connections="+strconv.Itoa(pgReserved))
	} else {
		f = append(f, "superuser_reserved_connections="+strconv.Itoa(pgReserved))
	}
	if pitrDays > 0 {
		dir := ArchiveDirOf(epoch)
		f = append(f,
			"archive_mode=on",
			"archive_timeout=60",
			"archive_command=test ! -f "+dir+"/%f && cp %p "+dir+"/.%f && mv "+
				dir+"/.%f "+dir+"/%f",
		)
	}
	return f
}

// pgStartScript runs as root ahead of the image's own entrypoint: it puts
// stackr.conf, stackr-owned.conf and their include lines in the data
// directory. (The replication lines of pg_hba.conf follow the standbys that
// exist: replLiveScript.) A first boot has no postgresql.conf until initdb
// has run, so an init script does it then. A start rewrites the two conf
// files only for a new container (its hostname differs from the one that
// wrote them), so a plain restart keeps what a later reload wrote; stackr-sync.conf
// (synchronous_standby_names) is only created (empty) when missing, so a recreated
// primary still waits for the standbys it did; a standby
// (STACKR_PG_REWRITE) rewrites them at every start, from an env its
// container is made again to keep current. The conf is written before the include, which is
// fatal when the file is missing.
const pgStartScript = `cat > /docker-entrypoint-initdb.d/00-stackr.sh <<'EOF'
printf '%s\n' "$STACKR_PG_CONF" > "$PGDATA/stackr.conf"
printf '%s\n' "$STACKR_PG_OWNED" > "$PGDATA/stackr-owned.conf"
: > "$PGDATA/stackr-sync.conf"
cat /etc/hostname > "$PGDATA/.stackr.conf.host"
printf "include 'stackr.conf'\ninclude 'stackr-owned.conf'\ninclude 'stackr-sync.conf'\n" >> "$PGDATA/postgresql.conf"
EOF
` + pgStartTail

// pgStartTail is what both starts share: with a data directory, the conf
// files and the includes, then the image's own entrypoint.
const pgStartTail = `d="$PGDATA"
if [ -s "$d/PG_VERSION" ]; then
if [ ! -e "$d/stackr.conf" ] || [ ! -e "$d/stackr-owned.conf" ] || [ "$(cat "$d/.stackr.conf.host" 2>/dev/null)" != "$(cat /etc/hostname)" ] || [ -n "$STACKR_PG_REWRITE" ]; then
printf '%s\n' "$STACKR_PG_CONF" > "$d/stackr.conf.new" && mv "$d/stackr.conf.new" "$d/stackr.conf"
printf '%s\n' "$STACKR_PG_OWNED" > "$d/stackr-owned.conf.new" && mv "$d/stackr-owned.conf.new" "$d/stackr-owned.conf"
cat /etc/hostname > "$d/.stackr.conf.host"
fi
[ -e "$d/stackr-sync.conf" ] || : > "$d/stackr-sync.conf"
chown postgres:postgres "$d/stackr.conf" "$d/stackr-owned.conf" "$d/stackr-sync.conf" "$d/.stackr.conf.host"
grep -qx "include 'stackr.conf'" "$d/postgresql.conf" || printf "include 'stackr.conf'\n" >> "$d/postgresql.conf"
grep -qx "include 'stackr-owned.conf'" "$d/postgresql.conf" || printf "include 'stackr-owned.conf'\n" >> "$d/postgresql.conf"
grep -qx "include 'stackr-sync.conf'" "$d/postgresql.conf" || printf "include 'stackr-sync.conf'\n" >> "$d/postgresql.conf"
fi
exec docker-entrypoint.sh "$@"`

// pgCommand is the container's command: the start script, then postgres with
// the owned flags. A named volume on a path the image lacks is root's, so
// the archive volume is handed to postgres first.
func pgCommand(i Instance) []string {
	script := pgStartScript
	if i.PITRDays > 0 {
		script = "chown postgres:postgres " + PGArchiveDir + "\n" + script
		if i.ArchiveEpoch > 0 { // the epoch's own directory, made on the archive volume
			d := ArchiveDirOf(i.ArchiveEpoch)
			script = "mkdir -p " + d + " && chown postgres:postgres " + d + "\n" + script
		}
	}
	cmd := []string{"sh", "-c", script, "sh", "postgres"}
	for _, f := range pgFlags(i.Major, i.PITRDays, i.ArchiveEpoch) {
		cmd = append(cmd, "-c", f)
	}
	return cmd
}

// ImageMajor is the major an image tag names (postgres:17-alpine is 17), 0
// when there is none.
// ponytail: a digest, "latest" or a custom tag reads as 0 and gets the 17
// behaviour (data path, allow_alter_system); pin a numeric tag for 18 and up.
func ImageMajor(image string) int {
	if strings.Contains(image, "@") {
		return 0
	}
	_, tag, ok := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	if !ok {
		return 0
	}
	n := 0
	for _, r := range tag {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// pgMount is where the data volume mounts: the image moved PGDATA under
// /var/lib/postgresql in 18 and expects the volume there.
func pgMount(major int) string {
	if major >= 18 {
		return "/var/lib/postgresql"
	}
	return "/var/lib/postgresql/data"
}

// pgDataDir is the cluster's data directory (PGDATA) in the container.
func pgDataDir(major int) string {
	if major >= 18 {
		return "/var/lib/postgresql/" + strconv.Itoa(major) + "/docker"
	}
	return "/var/lib/postgresql/data"
}

// PGDataPrefix is the data directory's path inside the data volume: "" up to
// 17, 18/docker from 18.
func PGDataPrefix(major int) string {
	return strings.Trim(strings.TrimPrefix(pgDataDir(major), pgMount(major)), "/")
}

func (pg) Definition() Definition {
	return Definition{
		Image:      "postgres:17",
		Port:       5432,
		Restart:    "no",
		Volumes:    func(major int) []string { return []string{pgMount(major)} },
		ArchiveDir: PGArchiveDir,
		Start:      pgCommand,
		Versioned:  true,
		Config: func(i Instance) []string {
			return []string{
				"POSTGRES_DB=" + i.AdminDB,
				"POSTGRES_USER=" + i.AdminUser,
				"POSTGRES_PASSWORD=" + i.AdminPassword,
				"STACKR_PG_CONF=" + pgConf(i),
				"STACKR_PG_OWNED=" + pgOwnedConf(i),
			}
		},
		PrimaryOutput: "url",
		SliceNoun:     "logical db",
		Backups:       []string{"dump", "base"},
		AdminDB:       "postgres",
		SliceName:     sqlIdent,
		SliceSep:      "_",
	}
}

func (pg) Ready(ctx context.Context, i Instance, x Tools) error {
	_, err := x.Exec(ctx, []string{"pg_isready", "-h", pgSocketDir, "-U", i.AdminUser})
	return err
}

// Provision is check-then-create, never a replayed CREATE: a CREATE failing
// on "already exists" makes postgres log the statement, password included.
func (e pg) Provision(ctx context.Context, i Instance, s Slice, x Tools) error {
	q := fmt.Sprintf(
		`SELECT (EXISTS (SELECT FROM pg_roles WHERE rolname = '%s'))::int || ',' || `+
			`(EXISTS (SELECT FROM pg_database WHERE datname = '%s'))::int`,
		s.User,
		s.Name,
	)
	out, err := x.Exec(ctx, append(e.psql(i, i.AdminDB, nil), "-tA", "-c", q))
	if err != nil {
		return err
	}
	role, db, _ := strings.Cut(strings.TrimSpace(out), ",")
	var stmts []string
	if role == "1" {
		stmts = append(stmts, fmt.Sprintf(`ALTER ROLE %q LOGIN PASSWORD '%s'`, s.User, s.Password))
	} else {
		stmts = append(stmts, fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, s.User, s.Password))
	}
	if db != "1" {
		stmts = append(
			stmts,
			fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, s.Name, s.User),
			fmt.Sprintf(`REVOKE CONNECT ON DATABASE %q FROM PUBLIC`, s.Name),
		)
	}
	if err = pgExecSQL(ctx, x, i, i.AdminDB, stmts); err != nil {
		return err
	}
	// pg_stat_statements is preloaded (pgFlags); each database needs the view
	return pgExecSQL(ctx, x, i, s.Name, []string{`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`})
}

func (e pg) Drop(ctx context.Context, i Instance, s Slice, x Tools) error {
	_, err := x.Exec(ctx, e.psql(i, i.AdminDB, []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, s.Name),
		fmt.Sprintf(`DROP ROLE IF EXISTS %q`, s.User),
	}))
	return err
}

// noStatementLogging keeps failing statements (which carry passwords) out of
// the instance's own log for the session.
const noStatementLogging = `SET log_min_error_statement = PANIC`

// Bind is check-then-create for a new user, as Provision is, then the
// grants; a moved access revokes first. It runs in the slice's database:
// schema, table and default privileges are per database.
func (e pg) Bind(
	ctx context.Context,
	i Instance,
	s Slice,
	g Grant,
	prev string,
	others []Grant,
	x Tools,
) error {
	var stmts []string
	if prev == "" {
		q := fmt.Sprintf(`SELECT (EXISTS (SELECT FROM pg_roles WHERE rolname = '%s'))::int`, g.User)
		out, err := x.Exec(ctx, append(e.psql(i, s.Name, nil), "-tA", "-c", q))
		if err != nil {
			return err
		}
		verb := "CREATE"
		if strings.TrimSpace(out) == "1" {
			verb = "ALTER"
		}
		stmts = append(stmts, fmt.Sprintf(`%s ROLE %q LOGIN PASSWORD '%s'`, verb, g.User, g.Password))
	} else {
		stmts = append(stmts, pgRevokes(s, g, prev, others)...)
	}
	stmts = append(stmts, pgGrants(s, g, others)...)
	err := pgExecSQL(ctx, x, i, s.Name, stmts)
	return err
}

func (e pg) Unbind(ctx context.Context, i Instance, s Slice, g Grant, x Tools) error {
	_, err := x.Exec(ctx, e.psql(i, s.Name, pgUnbind(s, g)))
	return err
}

// pgGrants is what g gets on s. Write: everything on the database, schema
// public and what is in it. Read: connect, usage and SELECT. Both reach the
// tables the owner and every writer create later (default privileges are
// per creating role), and a writer's later tables reach every other cred.
// ponytail: schema public only; another schema, functions and types are not
// granted, and a writer cannot ALTER or DROP another writer's table (owner
// only in postgres). A shared owner role (SET ROLE) is the upgrade.
func pgGrants(s Slice, g Grant, others []Grant) []string {
	u := quote(g.User)
	var out []string
	if g.Access == "write" {
		out = append(
			out,
			fmt.Sprintf(`GRANT ALL ON DATABASE %s TO %s`, quote(s.Name), u),
			`GRANT ALL ON SCHEMA public TO `+u,
			`GRANT ALL ON ALL TABLES IN SCHEMA public TO `+u,
			`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO `+u,
		)
	} else {
		out = append(
			out,
			fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, quote(s.Name), u),
			`GRANT USAGE ON SCHEMA public TO `+u,
			`GRANT SELECT ON ALL TABLES IN SCHEMA public TO `+u,
		)
	}
	out = append(out, reach(s.User, g)...)
	for _, o := range others {
		if o.Access == "write" {
			out = append(out, reach(o.User, g)...) // what o makes later reaches g
		}
		if g.Access == "write" {
			out = append(out, reach(g.User, o)...) // what g makes later reaches o
		}
	}
	return out
}

// reach is the default privileges that let the tables (and, for a writer,
// sequences) creator makes later reach g at its access.
func reach(creator string, g Grant) []string {
	on := func(what string) string {
		return fmt.Sprintf(
			`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT %s TO %s`,
			quote(creator),
			what,
			quote(g.User),
		)
	}
	if g.Access != "write" {
		return []string{
			on("SELECT ON TABLES"),
		}
	}
	return []string{
		on("ALL ON TABLES"),
		on("ALL ON SEQUENCES"),
	}
}

// pgRevokes takes back what g held at access prev, before pgGrants gives it
// the new access. A writer turning reader hands what it made to the owner,
// or it would keep writing its own tables.
func pgRevokes(s Slice, g Grant, prev string, others []Grant) []string {
	u := quote(g.User)
	off := func(creator, what string) string {
		return fmt.Sprintf(
			`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL ON %s FROM %s`,
			quote(creator),
			what,
			u,
		)
	}
	var out []string
	if prev == "write" {
		out = append(out, fmt.Sprintf(`REASSIGN OWNED BY %s TO %s`, u, quote(s.User)))
	}
	out = append(
		out,
		fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM %s`, quote(s.Name), u),
		`REVOKE ALL ON SCHEMA public FROM `+u,
		`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM `+u,
		`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM `+u,
		off(s.User, "TABLES"),
		off(s.User, "SEQUENCES"),
	)
	for _, o := range others {
		if o.Access == "write" {
			out = append(
				out,
				off(o.User, "TABLES"),
				off(o.User, "SEQUENCES"),
			)
		}
	}
	return out
}

// pgUnbind drops g's user: what it made passes to the owner, what it was
// granted (default privileges included) goes, then the role.
func pgUnbind(s Slice, g Grant) []string {
	return []string{
		fmt.Sprintf(`REASSIGN OWNED BY %s TO %s`, quote(g.User), quote(s.User)),
		`DROP OWNED BY ` + quote(g.User),
		`DROP ROLE IF EXISTS ` + quote(g.User),
	}
}

// quote is a SQL identifier in double quotes; names are sqlIdent-shaped, so
// there is nothing to escape.
func quote(ident string) string {
	return `"` + ident + `"`
}

// psql runs each statement as the superuser in one session on db,
// ON_ERROR_STOP. Quiet: without -q the SET prints its "SET" tag ahead of
// Provision's existence row, the check misreads it and re-provision CREATEs
// a live role. The admin connects over the container's own unix socket,
// where the image's pg_hba.conf trusts local connections: no password on the
// exec's argv or env.
func (pg) psql(i Instance, db string, stmts []string) []string {
	args := []string{
		"psql",
		"-h",
		pgSocketDir,
		"-q",
		"-U",
		i.AdminUser,
		"-d",
		db,
		"-v",
		"ON_ERROR_STOP=1",
		"-c",
		noStatementLogging,
		// the panel's own statements never wait for a standby: with
		// synchronous replication required and no standby, a removal's own
		// ALTER ROLE would wait for the standby it is removing
		"-c",
		"SET synchronous_commit = local",
	}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	return args
}

// pgSQL is psql reading its statements from stdin, for statements that carry
// a role's password: it never rides an argv. Returns the argv and the stdin.
func pgSQL(i Instance, db string, stmts []string) ([]string, string) {
	argv := pg{}.psql(i, db, nil)
	return append(argv, "-f", "-"), strings.Join(stmts, ";\n") + ";\n"
}

func pgExecSQL(ctx context.Context, x Tools, i Instance, db string, stmts []string) error {
	argv, in := pgSQL(i, db, stmts)
	_, err := x.ExecIn(ctx, argv, in)
	return err
}

// Bindings are a slice's outputs, named like a service tile's (lowercase,
// qualifier last): url, host and port follow the slice's url setting, and the
// _direct and _pooled forms always exist. The _read forms are the reads: they
// dial the read alias, which is the primary until a standby is in reach, so a
// stack file that refs them never changes when standbys arrive.
func (pg) Bindings(i Instance, s Slice) []Binding {
	conn := func(host string, port int) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s", s.User, s.Password, host, port, s.Name)
	}
	dHost, dPort := i.Host, i.Port
	// through the instance's pooler: transaction pooling, see pgbouncer.go
	pHost, pPort := PoolerAlias(i), PoolerPort
	host, port := dHost, dPort
	if s.Pooled {
		host, port = pHost, pPort
	}
	out := []Binding{
		{"url", conn(host, port), true, true},
		{"url_direct", conn(dHost, dPort), true, true},
		{"url_pooled", conn(pHost, pPort), true, true},
		{"host", host, false, true},
		{"host_direct", dHost, false, true},
		{"host_pooled", pHost, false, true},
		{"port", strconv.Itoa(port), false, false},
		{"port_direct", strconv.Itoa(dPort), false, false},
		{"port_pooled", strconv.Itoa(pPort), false, false},
		{"database", s.Name, false, false},
		{"user", s.User, false, false},
		{"password", s.Password, true, false},
	}
	// The read forms dial the read alias (see ReadAlias): a standby the
	// consumer can reach, else the primary. The pooled read forms dial the read
	// pooler alias: a standby's own pooler on its node, the primary's pooler on
	// the primary's. From another node they are the direct ones (Direct).
	rHost, rpHost := ReadAlias(i.Host), ReadPoolerAlias(i)
	rConn := conn(rHost, dPort)
	return append(out,
		Binding{"url_read", rConn, true, true},
		Binding{"url_read_direct", rConn, true, true},
		Binding{"url_read_pooled", conn(rpHost, PoolerPort), true, true},
		Binding{"host_read", rHost, false, true},
		Binding{"host_read_direct", rHost, false, true},
		Binding{"host_read_pooled", rpHost, false, true},
		Binding{"port_read", strconv.Itoa(dPort), false, false},
		Binding{"port_read_direct", strconv.Itoa(dPort), false, false},
		Binding{"port_read_pooled", strconv.Itoa(PoolerPort), false, false},
	)
}

// pgClusterMarker is the first line of a whole-instance dump. A dump without
// it is an old admin-only one (pg_dump of the admin DB) and restores as such.
const pgClusterMarker = `-- stackr dump format: cluster`

// Backup is pg_dumpall, not one pg_dump per slice database: it carries the
// roles (slice users, bound creds, password hashes), every database with
// its owner, ACLs and default privileges, in one plain SQL stream, so the
// restore is a wipe of every slice then one load with nothing to pair up.
// Per-database dumps would need a separate globals dump, a database list
// and a bundle format, and still lose the grants that live on the roles.
// exec keeps pg_dumpall's exit status as the script's.
func (pg) Backup(method string, i Instance) []string {
	if method == "base" {
		return pgBaseBackup(i)
	}
	if method != "dump" {
		return nil
	}
	script := `echo '` + pgClusterMarker + `'; exec pg_dumpall -h ` + pgSocketDir + ` -U "$0"`
	return []string{"sh", "-c", script, i.AdminUser}
}

// pgBaseBackup is a physical backup of the whole cluster as one gzipped tar
// on stdout, with no WAL inside (the archive has it). nil when PITR is off:
// a base without archiving cannot be restored to a point in time.
func pgBaseBackup(i Instance) []string {
	if i.PITRDays <= 0 {
		return nil
	}
	return []string{"pg_basebackup", "-h", pgSocketDir, "-U", i.AdminUser,
		"-D", "-", "-Ft", "-Xnone", "-z"}
}

// WalStartArgv is the argv whose stdout is the WAL segment being written
// now. A base backup starts at or after it, so WAL older than the oldest
// kept base's segment is safe to prune.
func (e pg) WalStartArgv(i Instance) []string {
	return append(e.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_walfile_name(pg_current_wal_lsn())`)
}

// WalStopArgv is the argv whose stdout is the WAL segment holding the end of
// the base backup that just finished: pg_backup_stop switched to the next
// segment, so one byte back is the completed one, which archives at once
// (the next one waits for activity or archive_timeout).
func (e pg) WalStopArgv(i Instance) []string {
	return append(e.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_walfile_name(pg_current_wal_lsn() - 1)`)
}

// InRecoveryArgv prints t while the server is still replaying WAL, f once it
// is a primary.
func (e pg) InRecoveryArgv(i Instance) []string {
	return append(e.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT pg_is_in_recovery()`)
}

// PGRestoreWalDir is where a point-in-time restore leaves the WAL it replays,
// under the data directory (the helper that fills the volume sees only that).
const PGRestoreWalDir = "stackr-restore-wal"

// PGWalDir is the cluster's own WAL directory: where a promoted restore's
// timeline history file lives.
func PGWalDir(major int) string { return pgDataDir(major) + "/pg_wal" }

// RecoveryConf is the postgresql.auto.conf text that makes the restored
// cluster replay the copied WAL and promote at at (zero = replay all of it).
// The timeline is the newest one the archive's history files name.
func (pg) RecoveryConf(i Instance, at time.Time) string {
	conf := "restore_command = 'cp " + pgDataDir(i.Major) + "/" + PGRestoreWalDir + "/%f %p'\n" +
		"recovery_target_timeline = 'latest'\n"
	if !at.IsZero() {
		conf += "recovery_target_time = '" + at.UTC().Format("2006-01-02 15:04:05.999999-07") + "'\n" +
			"recovery_target_action = 'promote'\n"
	}
	return conf
}

// SettleArgv is what a finished recovery leaves to clean: the WAL copy and
// the recovery settings (they do nothing outside recovery, but the promoted
// cluster should not carry a restore_command into a world without the dir).
// The settings come out of postgresql.auto.conf by hand: ALTER SYSTEM is off
// (allow_alter_system) from 17. The file is rewritten in place, so it stays
// postgres's own.
func (e pg) SettleArgv(i Instance) []string {
	script := `d="$0"; rm -rf "$d/` + PGRestoreWalDir + `"; f="$d/postgresql.auto.conf"
if [ -f "$f" ]; then
grep -Ev '^(restore_command|recovery_target_time|recovery_target_action|recovery_target_timeline)[ =]' "$f" > "$f.new"
[ $? -le 1 ] && cat "$f.new" > "$f"
rm -f "$f.new"
fi
exec "$@"`
	args := e.psql(i, i.AdminDB, []string{`SELECT pg_reload_conf()`})
	return append([]string{"sh", "-c", script, pgDataDir(i.Major)}, args...)
}

// ArchiverArgv prints pg_stat_archiver's failed_count.
func (e pg) ArchiverArgv(i Instance) []string {
	return append(e.psql(i, i.AdminDB, nil), "-tA", "-c", `SELECT failed_count FROM pg_stat_archiver`)
}

// DumpMarker is the first line of a whole-instance dump (flow.DumpMarker).
func (pg) DumpMarker() string { return pgClusterMarker }

// pgWipe drops every database and role but the target and the admin user,
// so the load starts from an empty instance. Sliced databases hold
// connections from running tiles; FORCE closes them.
const pgWipe = `SELECT format('DROP DATABASE %I WITH (FORCE)', datname) FROM pg_database
 WHERE datname NOT IN ('template0', 'template1', current_database()) \gexec
SELECT format('DROP OWNED BY %I', rolname) FROM pg_roles
 WHERE rolname !~ '^pg_' AND rolname <> current_user \gexec
SELECT format('DROP ROLE %I', rolname) FROM pg_roles
 WHERE rolname !~ '^pg_' AND rolname <> current_user \gexec
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;`

// Restore wipes, then restores: a bare psql < dump merges. sh, not bash
// (alpine images); the admin connects over the container's own socket, so no
// password rides the exec.
// The first line of the stream picks the path. A cluster dump re-creates the
// admin user, which exists and cannot be dropped, so its two statements are
// filtered out, from the globals section only (before the first \connect):
// after it a COPY row or a function body line can read the same. An old
// admin-only dump gets the old schema-only wipe.
func (pg) Restore(method, target string, i Instance) []string {
	if method != "dump" {
		return nil
	}
	script := `IFS= read -r first
if [ "$first" = '` + pgClusterMarker + `' ]; then
psql -h ` + pgSocketDir + ` -q -v ON_ERROR_STOP=1 -U "$0" -d "$1" >/dev/null <<'SQL' &&
` + pgWipe + `
SQL
LC_ALL=C awk -v u="$0" 'BEGIN { r = "^(CREATE|ALTER) ROLE \"?" u "\"?( |;)" }
/^\\connect / { data = 1 }
!data && $0 ~ r { next }
{ print }' | psql -h ` + pgSocketDir + ` -q -v ON_ERROR_STOP=1 -U "$0" -d "$1" >/dev/null
else
psql -h ` + pgSocketDir + ` -v ON_ERROR_STOP=1 -U "$0" -d "$1" ` +
		`-c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public' >/dev/null && ` +
		`{ printf '%s\n' "$first"; cat; } | psql -h ` + pgSocketDir + ` -v ON_ERROR_STOP=1 -U "$0" -d "$1" >/dev/null
fi`
	return []string{"sh", "-c", script, i.AdminUser, target}
}

// pgSeedFilter drops what ties a plain dump to its source's roles (owners,
// grants, session authorization) and the dump's own \restrict lines,
// outside COPY data, where a row can read the same. ponytail: a function
// body line starting with GRANT is dropped too; such a dump loads as
// pg_dump -Fc instead.
const pgSeedFilter = `/^COPY .* FROM stdin;$/ { copy = 1 }
copy { print; if ($0 == "\\.") copy = 0; next }
/^(ALTER .* OWNER TO |GRANT |REVOKE |ALTER DEFAULT PRIVILEGES |SET SESSION AUTHORIZATION )/ { next }
/^\\(un)?restrict / { next }
{ print }`

// SliceDump is pg_dump -Fc of slice s's database, as the instance's admin
// over the container's own socket (trusted there): no password in argv. It
// runs in the instance's container and its output is what Seed loads.
func (pg) SliceDump(i Instance, s Slice) []string {
	return []string{"pg_dump", "-h", pgSocketDir, "-Fc", "--no-owner", "--no-privileges", "-U", i.AdminUser, "-d", s.Name}
}

// Seed loads a pg_dump into slice s as its owner, so the slice's own cred
// and every bound one reach what it makes: no owners and no grants from
// the source, one transaction, the first error stops it. Notices stay out
// of stderr so a failure's tail is the error. A pg_dumpall or a -C dump
// (CREATE DATABASE, \connect) fails and says so.
// ponytail: an extension only a superuser may create fails the load.
func (pg) Seed(format string, _ Instance, s Slice) []string {
	// over the container's own socket, trusted there: no password rides the exec
	env := []string{"env", "PGOPTIONS=-c client_min_messages=warning"}
	switch format {
	case DumpCustom:
		return append(env, "pg_restore", "-h", pgSocketDir, "--no-owner", "--no-privileges", "--exit-on-error",
			"--single-transaction", "-U", s.User, "-d", s.Name)
	case DumpPlain:
		// psql runs in restricted mode under a key the dump cannot know: a
		// meta-command in it (\! runs a shell) fails the load.
		script := `k=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); ` +
			`{ printf '\\restrict %s\n' "$k"; LC_ALL=C awk '` + pgSeedFilter + `'; } | ` +
			`psql -h ` + pgSocketDir + ` -X -q -v ON_ERROR_STOP=1 --single-transaction -U "$0" -d "$1" -f - >/dev/null`
		return append(env, "sh", "-c", script, s.User, s.Name)
	}
	return nil
}
