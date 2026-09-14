package managedtiles

// Everything postgres knows about itself: how it cuts a logical database for
// a consumer, tears one down, reports readiness and per-database counters,
// and what a slice publishes. The registry (databases.go) points at these;
// nothing outside this file branches on the engine name.

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// pgProvision creates a logical database + owner role for the consumer.
func pgProvision(s *Service, ctx context.Context, instance, consumer *repo.Tile, name string, _ bool) (*repo.Provision, error) {
	// The instance joins its shared network with its alias so consumers in
	// other envs can resolve it; consumers join on their next deploy. Done
	// before the container is resolved: the first join rolls the task, and a
	// container id read before that roll is dead by the time the SQL runs.
	if err := s.joinSharedNet(ctx, instance); err != nil {
		return nil, err
	}
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return nil, fmt.Errorf("instance %s is not running", instance.Slug)
	}
	existing, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	base := consumer.Slug
	if name != "" {
		base = name
	}
	dbName := uniqueSliceName(SliceName(instance.Engine, base), existing, "_")
	p := &repo.Provision{
		ID:             uuid.NewString(),
		InstanceTileID: instance.ID,
		ConsumerTileID: consumer.ID,
		EnvID:          consumer.EnvironmentID,
		DBName:         dbName,
		DBUser:         dbName,
		DBPassword:     secrets.RandomHex(16),
		SecretName:     sliceSecret(instance, consumer),
		Status:         "active",
		CreatedAt:      time.Now().UTC(),
	}
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, p.DBUser, p.DBPassword),
		fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, p.DBName, p.DBUser),
		fmt.Sprintf(`REVOKE CONNECT ON DATABASE %q FROM PUBLIC`, p.DBName),
	}
	if err := s.psql(ctx, cid, instance, stmts); err != nil {
		return nil, err
	}
	if err := s.store.CreateProvision(ctx, p); err != nil {
		return nil, err
	}
	if err := s.SyncResource(ctx, instance, p); err != nil {
		return nil, err
	}
	return p, nil
}

// pgDrop destroys the logical database and its owner role.
func pgDrop(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision) error {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return fmt.Errorf("instance %s is not running", instance.Slug)
	}
	return s.psql(ctx, cid, instance, []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, p.DBName),
		fmt.Sprintf(`DROP ROLE IF EXISTS %q`, p.DBUser),
	})
}

// pgFork copies a logical database into one ForkSlice has already created
// empty on the same instance.
//
// Three things a naive `pg_dump | psql` gets wrong. Ownership: the dump has to
// run as the superuser to read every table, so --no-owner --no-acl strips the
// source role out and the restore connects as the *fork's* role, leaving every
// restored object owned by the fork, which is what lets it then run the
// migration being rehearsed. Extensions: a non-superuser restore cannot create
// them, so they are created here first as the superuser and the dump's own
// CREATE EXTENSION IF NOT EXISTS then no-ops. Comments: pg_dump follows an
// extension with COMMENT ON EXTENSION, which only its owner may run, and the
// fork's role is not it, so the restore dies at the first extension.
//
// --no-comments is the cheap way past that last one, and it drops
// the source's table/column comments with it. The alternative is restoring as
// the superuser and re-owning every object afterwards, which REASSIGN OWNED
// cannot do (the superuser owns system-pinned objects), so it means
// enumerating object classes by hand, and forgetting one fails a migration
// confusingly rather than losing a comment. Revisit if comments start
// mattering.
//
// pg_dump holds an MVCC snapshot, so the copy is consistent and does not block
// writers, but it does hold back vacuum on the source for its duration.
func pgFork(s *Service, ctx context.Context, instance *repo.Tile, src, dst *repo.Provision) error {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return fmt.Errorf("instance %s is not running", instance.Slug)
	}
	exts, err := s.pgExtensions(ctx, cid, instance, src.DBName)
	if err != nil {
		return err
	}
	if len(exts) > 0 {
		stmts := make([]string, 0, len(exts))
		for _, e := range exts {
			stmts = append(stmts, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %q`, e))
		}
		if err := s.psqlDB(ctx, cid, instance, dst.DBName, stmts); err != nil {
			return err
		}
	}
	// bash with pipefail, not sh: a pipeline reports only its last command's
	// status, so without it a pg_dump that dies halfway leaves psql to succeed
	// on a truncated stream and the fork looks fine, measured, it exits 0.
	// Both official images ship bash; one without it fails loudly here, which
	// is the right way to find that out.
	//
	// Passwords arrive as positional args ($0…) rather than inside the script
	// so they are not part of the command string.
	script := `PGPASSWORD="$0" pg_dump -U "$1" -d "$2" --no-owner --no-acl --no-comments | ` +
		`PGPASSWORD="$3" psql -v ON_ERROR_STOP=1 -U "$4" -d "$5" >/dev/null`
	out, err := s.execOn(ctx, instance, cid, []string{"bash", "-o", "pipefail", "-c", script,
		instance.DBPassword, instance.DBUser, src.DBName,
		dst.DBPassword, dst.DBUser, dst.DBName})
	if err != nil {
		return fmt.Errorf("pg_dump|psql: %s", strings.TrimSpace(out))
	}
	return nil
}

// pgExtensions lists the extensions installed in a database, minus plpgsql,
// which every database has from initdb and no dump recreates.
func (s *Service) pgExtensions(ctx context.Context, containerID string, instance *repo.Tile, db string) ([]string, error) {
	out, err := s.execOn(ctx, instance, containerID, []string{
		"env", "PGPASSWORD=" + instance.DBPassword,
		"psql", "-U", instance.DBUser, "-d", db, "-tA",
		"-c", `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`,
	})
	if err != nil {
		return nil, fmt.Errorf("psql: %s", strings.TrimSpace(out))
	}
	var exts []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			exts = append(exts, line)
		}
	}
	return exts, nil
}

// pgReady asks the instance itself whether it is accepting connections.
func pgReady(s *Service, ctx context.Context, instance *repo.Tile, containerID string) error {
	out, err := s.execOn(ctx, instance, containerID, []string{"pg_isready", "-U", instance.DBUser})
	if err != nil {
		return fmt.Errorf("pg_isready: %s", strings.TrimSpace(out))
	}
	return nil
}

// psql runs each statement via the instance's superuser; ON_ERROR_STOP so a
// failed CREATE surfaces instead of silently continuing. All -c statements
// share one session, so the logging guard above covers them all.
func (s *Service) psql(ctx context.Context, containerID string, instance *repo.Tile, stmts []string) error {
	return s.psqlDB(ctx, containerID, instance, "postgres", stmts)
}

// psqlDB is psql aimed at one named database rather than the instance's own,
// still as the superuser, since the only callers are engine-level work
// (extensions) the slice's own role is not allowed to do.
func (s *Service) psqlDB(ctx context.Context, containerID string, instance *repo.Tile, db string, stmts []string) error {
	args := []string{"env", "PGPASSWORD=" + instance.DBPassword, "psql", "-U", instance.DBUser, "-d", db, "-v", "ON_ERROR_STOP=1", "-c", noStatementLogging}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	out, err := s.execOn(ctx, instance, containerID, args)
	if err != nil {
		return fmt.Errorf("psql: %s", strings.TrimSpace(out))
	}
	return nil
}

// psqlLenient runs each statement without ON_ERROR_STOP, so an expected error
// (e.g. "role/database already exists" on an idempotent reconcile) doesn't
// abort the rest. Returns the combined output and the exec error, if any.
func (s *Service) psqlLenient(ctx context.Context, containerID string, instance *repo.Tile, stmts []string) (string, error) {
	args := []string{"env", "PGPASSWORD=" + instance.DBPassword, "psql", "-U", instance.DBUser, "-d", "postgres", "-c", noStatementLogging}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	out, err := s.execOn(ctx, instance, containerID, args)
	return strings.TrimSpace(out), err
}

// pgProvisionState reports whether the provision's role and database already
// exist, so reconcile only creates what is missing instead of replaying
// CREATEs that error (and would log their statements).
func (s *Service) pgProvisionState(ctx context.Context, containerID string, instance *repo.Tile, p *repo.Provision) (roleExists, dbExists bool, err error) {
	q := fmt.Sprintf(
		`SELECT (EXISTS (SELECT FROM pg_roles WHERE rolname = %s))::int || ',' || (EXISTS (SELECT FROM pg_database WHERE datname = %s))::int`,
		pgLit(p.DBUser), pgLit(p.DBName))
	out, err := s.execOn(ctx, instance, containerID, []string{
		"env", "PGPASSWORD=" + instance.DBPassword,
		"psql", "-U", instance.DBUser, "-d", "postgres", "-tA", "-c", q,
	})
	if err != nil {
		return false, false, fmt.Errorf("psql: %s", strings.TrimSpace(out))
	}
	state := strings.TrimSpace(out)
	return strings.HasPrefix(state, "1"), strings.HasSuffix(state, "1"), nil
}

// noStatementLogging keeps failing statements out of the instance's own log
// for the rest of the session. Our statements carry plaintext passwords
// (CREATE/ALTER ROLE), and postgres logs a failing statement verbatim at the
// default log_min_error_statement=ERROR, which is how credentials ended up
// in the container log. Session-local, superuser-only, first statement of
// every provisioning session.
const noStatementLogging = `SET log_min_error_statement = PANIC`

// pgEnsure idempotently recreates the role + database for a provision.
// It checks what exists first and only creates what is missing: replaying a
// CREATE that fails with "already exists" made postgres log the failing
// statement (password included) into the instance's own log on every
// consumer deploy. The ALTER ROLE always runs to re-sync the password to the
// row's creds; it succeeds silently, so nothing is logged.
func pgEnsure(s *Service, ctx context.Context, instance *repo.Tile, p *repo.Provision, w io.Writer) {
	// Keep the shared network + alias current so cross-env consumers resolve
	// it. A no-op once the spec carries it, so the reconcile does not roll.
	_ = s.joinSharedNet(ctx, instance)
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		_, _ = fmt.Fprintf(w, "warning: instance %s not running; skipping db reconcile (secret still set)\n", instance.Slug)
		return
	}
	roleExists, dbExists, err := s.pgProvisionState(ctx, cid, instance, p)
	if err != nil {
		_, _ = fmt.Fprintf(w, "note: db reconcile for %s: %v\n", p.DBName, err)
		return
	}
	var stmts []string
	if !roleExists {
		stmts = append(stmts, fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, p.DBUser, p.DBPassword))
	} else {
		stmts = append(stmts, fmt.Sprintf(`ALTER ROLE %q LOGIN PASSWORD '%s'`, p.DBUser, p.DBPassword))
	}
	if !dbExists {
		stmts = append(stmts, fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, p.DBName, p.DBUser))
	}
	if out, err := s.psqlLenient(ctx, cid, instance, stmts); err != nil {
		_, _ = fmt.Fprintf(w, "note: db reconcile for %s: %s\n", p.DBName, out)
	}
}

// pgOutputs publishes the connection url plus the PG* set libpq reads
// directly. Values are derived, not stored: host and port come from the
// instance tile, the rest from the provision row.
//
// requires_network: these hostnames only resolve on the instance's shared
// network, so the resolver has to pull the consumer onto it.
func pgOutputs(_ *Service, _ context.Context, instance *repo.Tile, p *repo.Provision) []repo.ResourceOutput {
	vars := pgVars(envnet.TileAlias(instance.ID), Engines[instance.Engine].Port, p.DBUser, p.DBPassword, p.DBName)
	out := make([]repo.ResourceOutput, 0, len(vars))
	for _, v := range vars {
		out = append(out, repo.ResourceOutput{
			Name: v.Name, Value: v.Value, Secret: v.Secret,
			RequiresNetwork: carriesHost(v.Name),
		})
	}
	return out
}

// pgSliceStats reads per-database counters for the named databases. Runs psql
// inside the container as the instance superuser, the same route provisioning
// uses, so nothing has to be reachable over the network. Returns what it
// could read; an unknown database is simply absent.
func pgSliceStats(s *Service, ctx context.Context, instance *repo.Tile, dbNames []string) (map[string]SliceStat, error) {
	cid, err := s.ContainerID(ctx, instance)
	if err != nil || cid == "" {
		return nil, err
	}
	// Unaligned, pipe-separated, tuples only: one line per database, trivially
	// parsed. pg_database_size needs the name quoted as a literal, not an ident.
	quoted := make([]string, 0, len(dbNames))
	for _, n := range dbNames {
		quoted = append(quoted, "'"+strings.ReplaceAll(n, "'", "''")+"'")
	}
	q := `SELECT d.datname, s.xact_commit + s.xact_rollback, pg_database_size(d.datname)
	      FROM pg_database d JOIN pg_stat_database s ON s.datname = d.datname
	      WHERE d.datname IN (` + strings.Join(quoted, ",") + `)`
	out, err := s.execOn(ctx, instance, cid, []string{
		"env", "PGPASSWORD=" + instance.DBPassword,
		"psql", "-U", instance.DBUser, "-d", "postgres", "-At", "-F", "|", "-c", q,
	})
	if err != nil {
		return nil, err
	}
	return parseSliceStats(out), nil
}

// parseSliceStats reads psql's unaligned output. Anything malformed is skipped
// rather than failing the whole sample, a missing number is a blank card, not
// a broken canvas.
func parseSliceStats(out string) map[string]SliceStat {
	stats := map[string]SliceStat{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		xacts, err1 := strconv.ParseUint(parts[1], 10, 64)
		size, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		stats[parts[0]] = SliceStat{Xacts: xacts, Size: size}
	}
	return stats
}

// pgVars is what libpq reads, spelled once. Two things publish it, the
// instance about itself (its own database and superuser) and a slice about
// itself (that consumer's database and role), with the same names and never
// the same values, so the list lives here and the callers supply the subject.
//
// hostsOnly names the entries whose value is a hostname; the slice path has
// to flag those as requiring the shared network.
func pgVars(host string, port int, user, pass, dbName string) []ConnVar {
	return []ConnVar{
		{"DATABASE_URL", fmt.Sprintf("postgres://%s:%s@%s:%d/%s", user, pass, host, port, dbName), true},
		{"PGHOST", host, false},
		{"PGPORT", strconv.Itoa(port), false},
		{"PGDATABASE", dbName, false},
		{"PGUSER", user, false},
		{"PGPASSWORD", pass, true},
	}
}

// carriesHost reports whether a pgVars entry embeds the hostname.
func carriesHost(name string) bool { return name == "DATABASE_URL" || name == "PGHOST" }

func pgConn(d *repo.Tile) []ConnVar {
	return pgVars(connHost(d), Engines[d.Engine].Port, d.DBUser, d.DBPassword, d.DBName)
}
