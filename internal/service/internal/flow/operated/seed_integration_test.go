//go:build integration

// Seed's argv against a real postgres:17: make test-integration.
package operated

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSeedAgainstPostgres(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	name := fmt.Sprintf("stkr-it-seed-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_USER=stackr", "-e", "POSTGRES_PASSWORD=admin", "postgres:17").CombinedOutput(); err != nil {
		t.Skipf("no postgres:17 container: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	i := Instance{AdminUser: "stackr", AdminPassword: "admin", AdminDB: "postgres"}
	run := func(argv []string, stdin []byte) (string, error) {
		cmd := exec.Command("docker", append([]string{"exec", "-i", name}, argv...)...)
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		if err != nil {
			err = fmt.Errorf("%w: %s", err, errb.String())
		}
		return out.String(), err
	}
	x := Tools{
		Exec:   func(_ context.Context, argv []string) (string, error) { return run(argv, nil) },
		ExecIn: func(_ context.Context, argv []string, in string) (string, error) { return run(argv, []byte(in)) },
	}
	ctx := context.Background()
	for end := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		// the image's init server answers on the socket, then restarts; only
		// the final server listens on TCP
		if _, err := run([]string{"env", "PGPASSWORD=" + i.AdminPassword, "psql", "-h", "127.0.0.1", "-U", i.AdminUser, "-d", "postgres", "-c", "SELECT 1"}, nil); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("postgres never came up")
		}
	}

	// the source: owned by a role the target does not have, with a grant
	src := []string{
		`CREATE ROLE alice`, `CREATE ROLE bob`, `CREATE DATABASE src OWNER alice`,
	}
	if _, err := run(pg{}.psql(i, "postgres", src), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := run(pg{}.psql(i, "src", []string{
		`SET ROLE alice`,
		`CREATE TABLE t (id serial PRIMARY KEY, v text)`,
		`INSERT INTO t (v) VALUES ('GRANT ALL ON t TO bob;'), ('plain')`,
		`GRANT SELECT ON t TO bob`,
	}), nil); err != nil {
		t.Fatal(err)
	}
	dump := func(format string) []byte {
		out, err := run([]string{"pg_dump", "-U", "stackr", "-F", format, "src"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return []byte(out)
	}
	custom, plain := dump("c"), dump("p")
	if f, _ := SniffDump(custom); f != DumpCustom {
		t.Fatalf("custom sniffed as %q", f)
	}
	if f, _ := SniffDump(plain); f != DumpPlain {
		t.Fatalf("plain sniffed as %q", f)
	}

	for n, c := range []struct {
		format string
		dump   []byte
	}{{DumpCustom, custom}, {DumpPlain, plain}} {
		s := Slice{Name: fmt.Sprintf("seed%d", n), User: fmt.Sprintf("seed%d", n), Password: "pw"}
		if err := (pg{}).Provision(ctx, i, s, x); err != nil {
			t.Fatal(err)
		}
		if _, err := run(pg{}.Seed(c.format, i, s), c.dump); err != nil {
			t.Fatalf("%s load: %v", c.format, err)
		}
		got, err := run(append(pg{}.psql(i, s.Name, nil), "-tA", "-c",
			`SELECT tableowner || ':' || (SELECT count(*) FROM t) FROM pg_tables WHERE tablename = 't'`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := s.User + ":2"; strings.TrimSpace(got) != want {
			t.Errorf("%s: owner:rows = %q, want %s", c.format, got, want)
		}
	}

	// a failing load keeps nothing
	s := Slice{Name: "seedbad", User: "seedbad", Password: "pw"}
	if err := (pg{}).Provision(ctx, i, s, x); err != nil {
		t.Fatal(err)
	}
	bad := []byte("CREATE TABLE kept (id int);\nSELECT nope;\n")
	if _, err := run(pg{}.Seed(DumpPlain, i, s), bad); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("bad load = %v, want the error", err)
	}
	got, _ := run(append(pg{}.psql(i, s.Name, nil), "-tA", "-c", `SELECT count(*) FROM pg_tables WHERE tablename = 'kept'`), nil)
	if strings.TrimSpace(got) != "0" {
		t.Errorf("a failed load kept its tables: %q", got)
	}

	// a meta-command in a plain dump does not run: \! would be a shell
	evil := []byte("CREATE TABLE kept (id int);\n\\! touch /tmp/seeded\n")
	if _, err := run(pg{}.Seed(DumpPlain, i, s), evil); err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Errorf("meta-command load = %v, want refused", err)
	}
	if _, err := run([]string{"test", "-e", "/tmp/seeded"}, nil); err == nil {
		t.Error("a dump's \\! ran a shell command")
	}
}
