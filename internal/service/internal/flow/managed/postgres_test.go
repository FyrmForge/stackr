package managed

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The statements per access, exactly: what a reviewer checks against the
// postgres docs, and what a change here has to say out loud.
func TestPostgresStatements(t *testing.T) {
	s := Slice{
		Name: "orders",
		User: "orders",
	}
	api := Grant{
		User:   "orders_api",
		Access: "write",
	}
	rep := Grant{
		User:   "orders_reporter",
		Access: "read",
	}
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "write, alone",
			got:  pgGrants(s, api, nil),
			want: []string{
				`GRANT ALL ON DATABASE "orders" TO "orders_api"`,
				`GRANT ALL ON SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL TABLES IN SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON TABLES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON SEQUENCES TO "orders_api"`,
			},
		},
		{
			name: "read, next to a writer",
			got:  pgGrants(s, rep, []Grant{api}),
			want: []string{
				`GRANT CONNECT ON DATABASE "orders" TO "orders_reporter"`,
				`GRANT USAGE ON SCHEMA public TO "orders_reporter"`,
				`GRANT SELECT ON ALL TABLES IN SCHEMA public TO "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
			},
		},
		{
			name: "write, next to a reader",
			got:  pgGrants(s, api, []Grant{rep}),
			want: []string{
				`GRANT ALL ON DATABASE "orders" TO "orders_api"`,
				`GRANT ALL ON SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL TABLES IN SCHEMA public TO "orders_api"`,
				`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON TABLES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public GRANT ALL ON SEQUENCES TO "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public GRANT SELECT ON TABLES TO "orders_reporter"`,
			},
		},
		{
			name: "write to read revokes and hands its tables to the owner",
			got:  pgRevokes(s, api, "write", []Grant{rep}),
			want: []string{
				`REASSIGN OWNED BY "orders_api" TO "orders"`,
				`REVOKE ALL ON DATABASE "orders" FROM "orders_api"`,
				`REVOKE ALL ON SCHEMA public FROM "orders_api"`,
				`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "orders_api"`,
				`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_api"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_api"`,
			},
		},
		{
			name: "read to write revokes, keeps nothing to hand over",
			got:  pgRevokes(s, rep, "read", []Grant{api}),
			want: []string{
				`REVOKE ALL ON DATABASE "orders" FROM "orders_reporter"`,
				`REVOKE ALL ON SCHEMA public FROM "orders_reporter"`,
				`REVOKE ALL ON ALL TABLES IN SCHEMA public FROM "orders_reporter"`,
				`REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public REVOKE ALL ON TABLES FROM "orders_reporter"`,
				`ALTER DEFAULT PRIVILEGES FOR ROLE "orders_api" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM "orders_reporter"`,
			},
		},
		{
			name: "unbind",
			got:  pgUnbind(s, rep),
			want: []string{
				`REASSIGN OWNED BY "orders_reporter" TO "orders"`,
				`DROP OWNED BY "orders_reporter"`,
				`DROP ROLE IF EXISTS "orders_reporter"`,
			},
		},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

// A dump covers every database of the instance (slices are separate
// databases), not the admin DB alone, and says so on its first line.
func TestPostgresBackupCoversTheCluster(t *testing.T) {
	i := Instance{AdminUser: "stackr", AdminPassword: "pw", AdminDB: "postgres"}
	got := pg{}.Backup("dump", i)
	if slices.Contains(got, "pg_dump") || slices.Contains(got, "-d") {
		t.Fatalf("admin-only dump: %v", got)
	}
	if !slices.Contains(got, "pg_dumpall") && !strings.Contains(strings.Join(got, " "), "pg_dumpall") {
		t.Fatalf("no pg_dumpall: %v", got)
	}
	if !strings.Contains(strings.Join(got, " "), pgClusterMarker) {
		t.Fatalf("no format marker: %v", got)
	}
	if strings.Contains(strings.Join(got[3:], " "), "pw") {
		t.Fatalf("password in script text: %v", got)
	}
	if v := (pg{}).Backup("volume", i); v != nil {
		t.Fatal("volume method has no dump argv")
	}
}

// Restore wipes every slice database and role, then loads; an old dump
// without the marker still takes the admin-only path.
func TestPostgresRestoreWipesAndLoadsTheCluster(t *testing.T) {
	i := Instance{AdminUser: "stackr", AdminPassword: "pw"}
	got := pg{}.Restore("dump", "postgres", i)
	if len(got) != 5 || got[0] != "sh" || got[2] == "" {
		t.Fatalf("argv: %v", got)
	}
	for _, want := range []string{pgClusterMarker, "DROP DATABASE", "DROP ROLE", "DROP SCHEMA public CASCADE"} {
		if !strings.Contains(got[2], want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(got[2], "pw") {
		t.Error("password in script text")
	}
	if !slices.Equal(got[3:], []string{"stackr", "postgres"}) {
		t.Errorf("positional args: %v", got[3:])
	}
}

// The admin-role filter is for the globals section only: after the first
// \connect a COPY row or a function body line that starts like a role
// statement is data. Runs the real script with a psql that records stdin.
func TestPostgresRestoreFilterSparesData(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "psql.in")
	fake := "#!/bin/sh\ncat >> \"$OUT\"\necho '==psql==' >> \"$OUT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	dump := strings.Join([]string{
		pgClusterMarker,
		`CREATE ROLE stackr;`,
		`ALTER ROLE stackr WITH SUPERUSER PASSWORD 'x';`,
		`CREATE ROLE shop_dev_web;`,
		`\connect shop_dev_web`,
		`COPY public.notes (id, body) FROM stdin;`,
		"1\tkept",
		`ALTER ROLE stackr is a note a user wrote`,
		`CREATE ROLE stackr;`,
		`\.`,
		``,
	}, "\n")
	args := pg{}.Restore("dump", "postgres", Instance{AdminUser: "stackr", AdminPassword: "pw"})
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "OUT="+out)
	cmd.Stdin = strings.NewReader(dump)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore script: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	loaded := string(b)
	for _, want := range []string{
		"CREATE ROLE shop_dev_web;",
		"ALTER ROLE stackr is a note a user wrote",
		"CREATE ROLE stackr;\n\\.",
	} {
		if !strings.Contains(loaded, want) {
			t.Errorf("the load lost %q", want)
		}
	}
	// the admin's own globals are filtered: it exists and cannot be dropped
	if strings.Contains(strings.SplitN(loaded, `\connect`, 2)[0], "ROLE stackr") {
		t.Error("the admin role statements reached the load")
	}
}

// A stack named pg made role names postgres reserves (pg_prod_appdb).
func TestSQLIdentAvoidsReservedPrefix(t *testing.T) {
	for in, want := range map[string]string{"pg_prod_appdb": "db_pg_prod_appdb", "pg-x": "db_pg_x", "pgx_a": "pgx_a", "9a": "db_9a", "shop_dev": "shop_dev"} {
		if got := sqlIdent(in); got != want {
			t.Errorf("sqlIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// A name past the limit is cut with a hash of the whole, so a branch of a
// long-named slice never lands on its source's name; short names are unchanged.
func TestLongNamesStayDistinct(t *testing.T) {
	src := "shop_development_" + strings.Repeat("orders", 8)
	br := src + "_br_a1b2"
	a, b := sqlIdent(src), sqlIdent(br)
	if a == b || len(a) > maxName || len(b) > maxName {
		t.Errorf("sqlIdent: %q vs %q", a, b)
	}
	if a != sqlIdent(src) {
		t.Error("the cut name is not stable")
	}
	if got := sqlIdent("shop_dev_" + strings.Repeat("x", 47)); len(got) != maxName || strings.Contains(got, "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx_") {
		t.Errorf("a name at the limit changed: %q", got)
	}
	if x, y := bucketName(strings.Repeat("a", 70)+"1"), bucketName(strings.Repeat("a", 70)+"2"); x == y || len(x) > maxName {
		t.Errorf("bucketName: %q vs %q", x, y)
	}
}
