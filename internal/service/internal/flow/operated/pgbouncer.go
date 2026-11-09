package operated

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Every postgres instance has a pgbouncer in a container of its own: one per
// member (an instance has one member today, its primary), on the instance's
// network, owned by the instance tile (its docker label, role "pooler", so
// Teardown takes it and Replicas never counts it). It pools only its own
// postgres, in transaction mode, and outlives the postgres container.
// Apps still dial postgres direct unless they take the pooled outputs;
// stackr's own work (backups, restores, branching, provisioning) never goes
// through it. stackr writes pgbouncer.ini and signals a reload; the image
// holds no code of ours (docker/pgbouncer).

const (
	// PoolerImage is stackr's pgbouncer image.
	// ponytail: the tag until the first CI push; then switch to the digest
	// the push printed (ghcr.io/fyrmforge/stackr-pgbouncer@sha256:...).
	PoolerImage = "ghcr.io/fyrmforge/stackr-pgbouncer:1"
	// PoolerPort is pgbouncer's listen port.
	PoolerPort = 6432
	// PoolerRole is the tile label role of the pooler container.
	PoolerRole = "pooler"

	poolerUser = "stackr_pooler"
	poolerSock = "/var/run/pgbouncer"
	poolerConf = "/etc/pgbouncer/pgbouncer.ini"
	// poolerHeadroom is what pgbouncer leaves of max_connections to stackr's
	// own direct connections (admin, backups, branching) and the reserved
	// slots.
	poolerHeadroom = 10
	// poolerAuthPool is the pool pgbouncer's auth_query uses, in the admin db.
	poolerAuthPool = 2
	poolerPoolMax  = 20
	poolerClients  = 1000
	// pgReserved is reserved_connections (superuser_reserved_connections
	// before 16): slots only stackr's admin role and replication can take,
	// so a full pool never locks stackr out.
	pgReserved = 5
	// defaultMaxConn is postgres's own default.
	defaultMaxConn = 100
)

// PoolerAlias is the pooler's name on the instance network.
func PoolerAlias(i Instance) string { return i.Host + "-pooler" }

func poolerName(i Instance) string { return "stackr-" + i.Host + "-pooler" }

// maxConnections is the instance's effective max_connections.
func maxConnections(i Instance) int {
	n, err := strconv.Atoi(strings.TrimSpace(PGEffective(i)["max_connections"]))
	if err != nil || n <= 0 {
		return defaultMaxConn
	}
	return n
}

// poolCaps splits what pgbouncer may hold of postgres: max_connections less
// the headroom, the auth pool first, the rest shared evenly by the n slice
// databases. The total, auth pool + n x per, stays within the budget unless
// the budget cannot hold one connection each.
func poolCaps(maxConn, n int) (auth, per int) {
	budget := max(maxConn-poolerHeadroom, poolerAuthPool+max(n, 1))
	if n <= 0 {
		return poolerAuthPool, 0
	}
	return poolerAuthPool, max((budget-poolerAuthPool)/n, 1)
}

// poolerIni is pgbouncer.ini for i with one entry per slice database dbs
// (sorted, so the file is stable). Each database is capped so that every
// pool together fits under max_connections; a database not listed is
// refused by the pooler. The admin database is listed only for auth_query.
// No secret is in it: stackr_pooler's password is in the userlist.
func poolerIni(i Instance, dbs []string) string {
	dbs = slices.Compact(slices.Sorted(slices.Values(dbs)))
	dbs = slices.DeleteFunc(dbs, func(d string) bool { return d == i.AdminDB })
	auth, per := poolCaps(maxConnections(i), len(dbs))
	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	line("; written by stackr; edits here are overwritten")
	line("[databases]")
	target := fmt.Sprintf("host=%s port=%d", i.Host, i.Port)
	line("%s = %s pool_size=%d max_db_connections=%d", i.AdminDB, target, auth, auth)
	for _, d := range dbs {
		line("%s = %s pool_size=%d max_db_connections=%d", d, target, min(per, poolerPoolMax), per)
	}
	line("")
	line("[pgbouncer]")
	line("listen_addr = *")
	line("listen_port = %d", PoolerPort)
	line("unix_socket_dir = %s", poolerSock)
	line("auth_type = scram-sha-256")
	line("auth_file = /etc/pgbouncer/userlist.txt")
	line("auth_user = %s", poolerUser)
	line("auth_dbname = %s", i.AdminDB)
	line("auth_query = SELECT usename, passwd FROM %s.get_auth($1)", poolerUser)
	line("admin_users = %s", poolerUser)
	line("pool_mode = transaction")
	line("max_prepared_statements = 200")
	line("ignore_startup_parameters = extra_float_digits,options")
	line("max_client_conn = %d", poolerClients)
	return b.String()
}

// poolerStartScript writes the userlist from the env (the password never
// rides an argv) and the ini once: a reload rewrites the ini in the
// container's own filesystem, which a plain restart keeps.
const poolerStartScript = `umask 077
[ -s ` + poolerConf + ` ] || printf '%s\n' "$STACKR_PGB_INI" > ` + poolerConf + `
printf '"` + poolerUser + `" "%s"\n' "$STACKR_PGB_PASS" > /etc/pgbouncer/userlist.txt
exec pgbouncer ` + poolerConf

// poolerSpec is the pooler container: non-root, no capabilities, on the
// instance network under its own alias, restarted unless stopped by hand.
func poolerSpec(i Instance, ini, network string) docker.ContainerSpec {
	return docker.ContainerSpec{
		Name:     poolerName(i),
		Image:    PoolerImage,
		Cmd:      []string{"sh", "-c", poolerStartScript},
		Env:      []string{"STACKR_PGB_INI=" + ini, "STACKR_PGB_PASS=" + i.PoolerPassword},
		Networks: []docker.NetAttach{{Name: network, Aliases: []string{PoolerAlias(i)}}},
		User:     "pgbouncer",
		CapDrop:  []string{"ALL"},
		Restart:  "unless-stopped",
		HealthCmd: "pg_isready -q -h " + poolerSock + " -p " +
			strconv.Itoa(PoolerPort),
		HealthIntervalS: 10,
	}
}

// poolerReload is the argv that swaps the ini in a running pooler and
// signals pgbouncer (pid 1) to read it.
func poolerReload(ini string) []string {
	script := `printf '%s\n' "$0" > ` + poolerConf + `.new && mv ` + poolerConf + `.new ` + poolerConf + ` && kill -HUP 1`
	return []string{"sh", "-c", script, ini}
}

// poolerConsole is the argv of one admin console command, as stackr_pooler
// over pgbouncer's unix socket; the password is the container's env.
func poolerConsole(cmd string) []string {
	return []string{"sh", "-c", `PGPASSWORD="$STACKR_PGB_PASS" exec psql -h ` + poolerSock +
		` -p ` + strconv.Itoa(PoolerPort) + ` -U ` + poolerUser + ` -d pgbouncer -tA -F '|' -c "$0"`, cmd}
}

// poolerAuthSQL makes the role pgbouncer logs in with and the one function
// it asks for the other roles' password verifiers (auth_query +
// auth_dbname): the lookup lives in the admin database only, so a new
// binding needs no pooler config. The function is SECURITY DEFINER, owned by
// the admin superuser, pinned to pg_catalog, and executable by the pooler
// role alone. Every statement is repeatable. roleExists picks CREATE or
// ALTER, as Provision does: a replayed CREATE failing on "already exists"
// would make postgres log the statement, password included.
func poolerAuthSQL(roleExists bool, password string) []string {
	verb := "CREATE"
	if roleExists {
		verb = "ALTER"
	}
	return []string{
		fmt.Sprintf(`%s ROLE %s LOGIN PASSWORD '%s'`, verb, poolerUser, password),
		`CREATE SCHEMA IF NOT EXISTS ` + poolerUser,
		`CREATE OR REPLACE FUNCTION ` + poolerUser + `.get_auth(p_usename text) ` +
			`RETURNS TABLE (usename name, passwd text) LANGUAGE sql SECURITY DEFINER ` +
			`SET search_path = pg_catalog AS $$ ` +
			`SELECT rolname::name, rolpassword FROM pg_catalog.pg_authid WHERE rolname = p_usename AND rolcanlogin $$`,
		`REVOKE ALL ON FUNCTION ` + poolerUser + `.get_auth(text) FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA ` + poolerUser + ` TO ` + poolerUser,
		`GRANT EXECUTE ON FUNCTION ` + poolerUser + `.get_auth(text) TO ` + poolerUser,
	}
}

// ensurePoolerAuth creates or re-syncs the pooler's role and lookup function
// in the admin database, then proves them: it logs in as the pooler role
// over the network (the password is checked) and calls the function. A
// missing function otherwise shows only as pgbouncer's "config error".
func (e pg) ensurePoolerAuth(ctx context.Context, i Instance, x Tools) error {
	q := `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + poolerUser + `')::int`
	out, err := x.Exec(ctx, append(e.psql(i, i.AdminDB, nil), "-tA", "-c", q))
	if err != nil {
		return err
	}
	if err := pgExecSQL(ctx, x, i, i.AdminDB, poolerAuthSQL(strings.TrimSpace(out) == "1", i.PoolerPassword)); err != nil {
		return err
	}
	return e.verifyPoolerAuth(ctx, i, x)
}

func (pg) verifyPoolerAuth(ctx context.Context, i Instance, x Tools) error {
	script := `read -r PGPASSWORD; export PGPASSWORD; exec psql -h "$0" -U ` + poolerUser + ` -d "$1" -tA ` +
		`-c "SELECT count(*) FROM ` + poolerUser + `.get_auth('$2')"`
	out, err := x.ExecIn(ctx, []string{"sh", "-c", script, i.Host, i.AdminDB, i.AdminUser}, i.PoolerPassword+"\n")
	if err != nil || strings.TrimSpace(out) != "1" {
		return errors.New("the pooler's auth lookup does not work: " + poolerUser + " cannot log in or call " + poolerUser + ".get_auth")
	}
	return nil
}

// sliceDBs are the databases of the instance's slices.
func (f *Flow) sliceDBs(ctx context.Context, m store.OperatedInstance) ([]string, error) {
	ps, err := f.Instances.ByInstance(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, p := range ps {
		dbs = append(dbs, p.DBName)
	}
	return dbs, nil
}

// EnsurePooler is the instance's pooler after its deploy: the auth role and
// function (also for an instance made before poolers), the pooler container
// made if it is missing, its config rewritten and reloaded if it is there.
// pull makes the pooler image available on the node. A postgres that is not
// there or an engine that is not postgres is nothing to do.
func (f *Flow) EnsurePooler(ctx context.Context, it store.Tile, pull func(ctx context.Context, ref string) error, log io.Writer) error {
	return f.pooler(ctx, it, pull, log)
}

// SyncPooler rewrites and reloads the pooler's config when one runs (a slice
// came or went, max_connections moved); it never makes one.
func (f *Flow) SyncPooler(ctx context.Context, it store.Tile) error {
	return f.pooler(ctx, it, nil, io.Discard)
}

func (f *Flow) pooler(ctx context.Context, it store.Tile, pull func(context.Context, string) error, log io.Writer) error {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return err
	}
	pe, ok := e.(pg)
	if !ok {
		return nil
	}
	cs, err := f.Tiles.Role(ctx, it, PoolerRole)
	if err != nil {
		return err
	}
	if len(cs) == 0 && pull == nil {
		return nil
	}
	if m.PoolerPassword == "" {
		if m, err = f.Instances.SetPoolerPassword(ctx, m, operated.Password()); err != nil {
			return err
		}
	}
	i := f.withNodeMem(ctx, it, f.facts(ctx, it, m, e.Definition()))
	x := f.tools(ctx, it, m)
	if err := pe.ensurePoolerAuth(ctx, i, x); err != nil {
		return err
	}
	dbs, err := f.sliceDBs(ctx, m)
	if err != nil {
		return err
	}
	ini := poolerIni(i, dbs)
	if len(cs) == 0 {
		net := operated.Network(m.ID)
		if err := f.Envs.Shared(ctx, net); err != nil {
			return err
		}
		if err := pull(ctx, PoolerImage); err != nil {
			return fmt.Errorf("pull %s: %w", PoolerImage, err)
		}
		logf(log, "starting the pooler for %s\n", it.Slug)
		_, err = f.Tiles.StartRole(ctx, it, PoolerRole, poolerSpec(i, ini, net), log)
		return err
	}
	c := cs[0]
	if c.State != "running" {
		if err := f.Tiles.StartContainer(ctx, it.ID, c.ID); err != nil {
			return err
		}
	}
	if _, err := f.Tiles.Exec(ctx, it.ID, c.ID, poolerReload(ini)); err != nil {
		return fmt.Errorf("reload the pooler: %w", err)
	}
	return nil
}

// PoolerStatus is the pooler as the drawer shows it. Word: none | running |
// stopped | unhealthy. Active and Waiting are the client connections in use
// and queued, summed over its pools; Counted says the console answered.
type PoolerStatus struct {
	Word            string
	Host            string
	Port            int
	Active, Waiting int
	Counted         bool
}

// Pooler reads the instance's pooler: the container's state and, when it
// runs, SHOW POOLS over the admin console.
func (f *Flow) Pooler(ctx context.Context, it store.Tile) (PoolerStatus, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return PoolerStatus{}, err
	}
	if _, ok := e.(pg); !ok {
		return PoolerStatus{}, errs.Invalidf("engine", "%s has no pooler; only postgres does", m.Engine)
	}
	s := PoolerStatus{Word: "none", Host: PoolerAlias(f.facts(ctx, it, m, e.Definition())), Port: PoolerPort}
	cs, err := f.Tiles.Role(ctx, it, PoolerRole)
	if err != nil || len(cs) == 0 {
		return s, err
	}
	switch c := cs[0]; {
	case c.Health == "unhealthy":
		s.Word = "unhealthy"
	case c.State == "running":
		s.Word = "running"
	default:
		s.Word = "stopped"
	}
	if s.Word == "running" {
		if out, err := f.Tiles.Exec(ctx, it.ID, cs[0].ID, poolerConsole("SHOW POOLS")); err == nil {
			s.Active, s.Waiting = parsePools(out)
			s.Counted = true
		}
	}
	return s, nil
}

// parsePools sums cl_active and cl_waiting (the third and fourth columns of
// SHOW POOLS, which every version keeps) over the pools, pgbouncer's own
// console database aside.
func parsePools(out string) (active, waiting int) {
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Split(l, "|")
		if len(p) < 4 || p[0] == "pgbouncer" {
			continue
		}
		a, err1 := strconv.Atoi(p[2])
		w, err2 := strconv.Atoi(p[3])
		if err1 == nil && err2 == nil {
			active, waiting = active+a, waiting+w
		}
	}
	return active, waiting
}
