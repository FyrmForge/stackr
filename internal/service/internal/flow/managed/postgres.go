package managed

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type pg struct{}

func (pg) Definition() Definition {
	return Definition{
		Image:   "postgres:17",
		Port:    5432,
		Volumes: []string{"/var/lib/postgresql/data"},
		Config: func(i Instance) []string {
			return []string{
				"POSTGRES_DB=" + i.AdminDB,
				"POSTGRES_USER=" + i.AdminUser,
				"POSTGRES_PASSWORD=" + i.AdminPassword,
			}
		},
		PrimaryOutput: "DATABASE_URL",
		SliceNoun:     "logical db",
		Backups:       []string{"dump"},
		AdminDB:       "postgres",
		SliceName:     sqlIdent,
		SliceSep:      "_",
	}
}

func (pg) Ready(ctx context.Context, i Instance, x Tools) error {
	_, err := x.Exec(ctx, []string{"pg_isready", "-U", i.AdminUser})
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
	_, err = x.Exec(ctx, e.psql(i, i.AdminDB, stmts))
	return err
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
	_, err := x.Exec(ctx, e.psql(i, s.Name, stmts))
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
// a live role.
func (pg) psql(i Instance, db string, stmts []string) []string {
	args := []string{
		"env",
		"PGPASSWORD=" + i.AdminPassword,
		"psql",
		"-q",
		"-U",
		i.AdminUser,
		"-d",
		db,
		"-v",
		"ON_ERROR_STOP=1",
		"-c",
		noStatementLogging,
	}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	return args
}

func (pg) Bindings(i Instance, s Slice) []Binding {
	return []Binding{
		{"DATABASE_URL", fmt.Sprintf("postgres://%s:%s@%s:%d/%s", s.User, s.Password, i.Host, i.Port, s.Name), true, true},
		{"PGHOST", i.Host, false, true},
		{"PGPORT", strconv.Itoa(i.Port), false, false},
		{"PGDATABASE", s.Name, false, false},
		{"PGUSER", s.User, false, false},
		{"PGPASSWORD", s.Password, true, false},
	}
}

func (pg) Backup(method string, i Instance) []string {
	if method != "dump" {
		return nil
	}
	return []string{"env", "PGPASSWORD=" + i.AdminPassword, "pg_dump", "-U", i.AdminUser, "-d", i.AdminDB}
}

// Restore wipes, then restores: a bare psql < dump merges. sh, not bash
// (alpine images); the password is a positional arg, never script text.
func (pg) Restore(method, target string, i Instance) []string {
	if method != "dump" {
		return nil
	}
	script := `PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" ` +
		`-c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public' >/dev/null && ` +
		`PGPASSWORD="$0" psql -v ON_ERROR_STOP=1 -U "$1" -d "$2" >/dev/null`
	return []string{"sh", "-c", script, i.AdminPassword, i.AdminUser, target}
}
