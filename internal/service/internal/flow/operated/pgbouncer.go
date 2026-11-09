package operated

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

	poolerUser  = "stackr_pooler"
	poolerProbe = "stackr_pooler_probe"
	poolerSock  = "/var/run/pgbouncer"
	poolerConf  = "/etc/pgbouncer/pgbouncer.ini"
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
// databases. The total, auth pool + n x per, never exceeds the budget: when
// the budget cannot give every database one connection it is an error, and
// the caller refuses the change that caused it.
func poolCaps(maxConn, n int) (auth, per int, err error) {
	budget := maxConn - poolerHeadroom
	if n <= 0 {
		return poolerAuthPool, 0, nil
	}
	if budget-poolerAuthPool < n {
		return 0, 0, errs.Invalidf("max_connections", "max_connections %d leaves %d connections for the pooler, "+
			"not enough for %d databases; raise max_connections or remove a slice", maxConn, max(budget-poolerAuthPool, 0), n)
	}
	return poolerAuthPool, (budget - poolerAuthPool) / n, nil
}

// poolerIni is pgbouncer.ini for i with one entry per slice database dbs
// (sorted, so the file is stable). Each database is capped so that every
// pool together fits under max_connections; a database not listed is
// refused by the pooler. The admin database is listed only for auth_query.
// No secret is in it: stackr_pooler's password is in the userlist.
func poolerIni(i Instance, dbs []string) (string, error) {
	dbs = slices.Compact(slices.Sorted(slices.Values(dbs)))
	dbs = slices.DeleteFunc(dbs, func(d string) bool { return d == i.AdminDB })
	auth, per, err := poolCaps(maxConnections(i), len(dbs))
	if err != nil {
		return "", err
	}
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
	return b.String(), nil
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
// role alone, and it answers only for plain login roles (no superuser,
// replication or bypassrls, never the pooler role): the admin's verifier is
// out of its reach. Every statement is repeatable. roleExists picks CREATE or
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
			`SELECT rolname::name, rolpassword FROM pg_catalog.pg_authid WHERE rolname = p_usename AND rolcanlogin AND NOT rolsuper AND NOT rolreplication ` +
			`AND NOT rolbypassrls AND rolname <> '` + poolerUser + `' $$`,
		// a plain login role with no password, so nobody can log in as it: what
		// the lookup is proved on
		`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + poolerProbe + `') THEN ` +
			`CREATE ROLE ` + poolerProbe + ` LOGIN NOSUPERUSER NOREPLICATION NOBYPASSRLS; END IF; END $$`,
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

// verifyPoolerAuth logs in as the pooler role over the network (so its
// password is checked) and asks the lookup for the probe role, which it must
// find, and for the admin and the pooler role, which it must not.
func (pg) verifyPoolerAuth(ctx context.Context, i Instance, x Tools) error {
	n := func(r string) string { return `(SELECT count(*) FROM ` + poolerUser + `.get_auth('` + r + `'))` }
	script := `read -r PGPASSWORD; export PGPASSWORD; exec psql -h "$0" -U ` + poolerUser + ` -d "$1" -tA ` +
		`-c "SELECT ` + n(poolerProbe) + `||','||` + n("$2") + `||','||` + n(poolerUser) + `"`
	out, err := x.ExecIn(ctx, []string{"sh", "-c", script, i.Host, i.AdminDB, i.AdminUser}, i.PoolerPassword+"\n")
	if err != nil || strings.TrimSpace(out) != "1,0,0" {
		return errors.New("the pooler's auth lookup does not work: " + poolerUser + " cannot log in, or " + poolerUser + ".get_auth answers wrongly")
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

// poolerLock serialises the pooler's config of one instance: the snapshot
// of its databases, the write and the reload are one step, so a slower job
// never writes a stale list over a newer one.
func (f *Flow) poolerLock(instanceID string) *sync.Mutex {
	m, _ := f.poolerMu.LoadOrStore(instanceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// EnsurePooler is the instance's pooler after its deploy: the auth role and
// function (also for an instance made before poolers), the pooler container
// made if it is missing, its config rewritten and reloaded if it is there.
// A failure is the deploy's: it can be retried, and nothing publishes a
// pooled address for a pooler that is not there.
func (f *Flow) EnsurePooler(ctx context.Context, it store.Tile, log io.Writer) error {
	return f.pooler(ctx, it, true, log)
}

// SyncPooler is EnsurePooler for a change after the deploy (a slice came,
// max_connections moved): it makes the pooler when it is missing.
func (f *Flow) SyncPooler(ctx context.Context, it store.Tile) error {
	return f.pooler(ctx, it, true, io.Discard)
}

// refreshPooler rewrites a pooler that runs and makes none: for a slice
// going, and an instance being torn down, which must not grow a container.
func (f *Flow) refreshPooler(ctx context.Context, it store.Tile) error {
	return f.pooler(ctx, it, false, io.Discard)
}

// CheckPoolBudget refuses a change the pooler's connection budget cannot
// hold: settings (the overrides that would be stored) and extra more slice
// databases must leave every database at least one connection.
func (f *Flow) CheckPoolBudget(ctx context.Context, it store.Tile, settings map[string]string, extra int) error {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return err
	}
	if _, ok := e.(pg); !ok {
		return nil
	}
	dbs, err := f.sliceDBs(ctx, m)
	if err != nil {
		return err
	}
	i := f.facts(ctx, it, m, e.Definition())
	if settings != nil {
		i.Settings = settings
	}
	_, _, err = poolCaps(maxConnections(i), len(dbs)+extra)
	return err
}

// liveMaxConnections is the max_connections postgres runs with, which a
// stored change does not touch until the restart: the pool caps follow it,
// never the stored value. Unreadable = the effective one.
func liveMaxConnections(ctx context.Context, i Instance, x Tools) Instance {
	live, _, err := pgLive(ctx, i, x, []string{"max_connections"})
	if v, ok := live["max_connections"]; err == nil && ok {
		i.Settings = maps.Clone(i.Settings)
		if i.Settings == nil {
			i.Settings = map[string]string{}
		}
		i.Settings["max_connections"] = v.Value
	}
	return i
}

func (f *Flow) pooler(ctx context.Context, it store.Tile, create bool, log io.Writer) error {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return err
	}
	pe, ok := e.(pg)
	if !ok {
		return nil
	}
	mu := f.poolerLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	if m, err = f.Instances.Get(ctx, m.ID); err != nil { // read under the lock
		return err
	}
	cs, err := f.Tiles.Role(ctx, it, PoolerRole)
	if err != nil {
		return err
	}
	if len(cs) == 0 && !create {
		return nil
	}
	if m.PoolerPassword == "" {
		if m, err = f.Instances.SetPoolerPassword(ctx, m, operated.Password()); err != nil {
			return err
		}
	}
	x := f.tools(ctx, it, m)
	i := liveMaxConnections(ctx, f.withNodeMem(ctx, it, f.facts(ctx, it, m, e.Definition())), x)
	if err := pe.ensurePoolerAuth(ctx, i, x); err != nil {
		return err
	}
	dbs, err := f.sliceDBs(ctx, m)
	if err != nil {
		return err
	}
	ini, err := poolerIni(i, dbs)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return f.startPooler(ctx, it, m, i, ini, log)
	}
	c := cs[0]
	if c.State != "running" {
		if err := f.Tiles.StartContainer(ctx, it.ID, c.ID); err != nil {
			return err
		}
		// the start races pgbouncer's own startup: reload once it is healthy
		if err := f.waitPooler(ctx, it); err != nil {
			return err
		}
	}
	if _, err := f.Tiles.Exec(ctx, it.ID, c.ID, poolerReload(ini)); err != nil {
		return fmt.Errorf("reload the pooler: %w", err)
	}
	return nil
}

func (f *Flow) startPooler(ctx context.Context, it store.Tile, m store.OperatedInstance, i Instance, ini string, log io.Writer) error {
	if f.Pull == nil {
		return fmt.Errorf("%s: no way to pull %s", it.Slug, PoolerImage)
	}
	nctx, err := f.Tiles.OnNode(ctx, it)
	if err != nil {
		return err
	}
	net := operated.Network(m.ID)
	if err := f.Envs.Shared(nctx, net); err != nil {
		return err
	}
	if err := f.Pull(nctx, PoolerImage); err != nil {
		return fmt.Errorf("pull %s: %w", PoolerImage, err)
	}
	logf(log, "starting the pooler for %s\n", it.Slug)
	_, err = f.Tiles.StartRole(ctx, it, PoolerRole, poolerSpec(i, ini, net), log)
	return err
}

// waitPooler waits for the pooler container to be running and healthy.
func (f *Flow) waitPooler(ctx context.Context, it store.Tile) error {
	wait, poll := f.ReadyWait, f.ReadyPoll
	if wait == 0 {
		wait = time.Minute
	}
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		cs, err := f.Tiles.Role(ctx, it, PoolerRole)
		if err == nil && len(cs) > 0 && cs[0].State == "running" && cs[0].Health != "starting" && cs[0].Health != "unhealthy" {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the pooler did not become healthy after its start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
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
