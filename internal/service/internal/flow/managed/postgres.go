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
			return []string{"POSTGRES_DB=" + i.AdminDB, "POSTGRES_USER=" + i.AdminUser, "POSTGRES_PASSWORD=" + i.AdminPassword}
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
	q := fmt.Sprintf(`SELECT (EXISTS (SELECT FROM pg_roles WHERE rolname = '%s'))::int || ',' || `+
		`(EXISTS (SELECT FROM pg_database WHERE datname = '%s'))::int`, s.User, s.Name)
	out, err := x.Exec(ctx, append(e.psql(i, nil), "-tA", "-c", q))
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
		stmts = append(stmts,
			fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, s.Name, s.User),
			fmt.Sprintf(`REVOKE CONNECT ON DATABASE %q FROM PUBLIC`, s.Name))
	}
	_, err = x.Exec(ctx, e.psql(i, stmts))
	return err
}

func (e pg) Drop(ctx context.Context, i Instance, s Slice, x Tools) error {
	_, err := x.Exec(ctx, e.psql(i, []string{
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, s.Name),
		fmt.Sprintf(`DROP ROLE IF EXISTS %q`, s.User),
	}))
	return err
}

// noStatementLogging keeps failing statements (which carry passwords) out of
// the instance's own log for the session.
const noStatementLogging = `SET log_min_error_statement = PANIC`

// psql runs each statement as the superuser in one session, ON_ERROR_STOP.
func (pg) psql(i Instance, stmts []string) []string {
	args := []string{"env", "PGPASSWORD=" + i.AdminPassword, "psql", "-U", i.AdminUser,
		"-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", noStatementLogging}
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
