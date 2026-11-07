//go:build integration

// A branch's argv against a real postgres:17: make test-integration.
package managed

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The source's pg_dump, piped into the branch's Seed: the row arrives, the
// branch owns it, and the branch's own cred cannot connect to the source.
func TestBranchAgainstPostgres(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	name := fmt.Sprintf("stkr-it-branch-%d", time.Now().UnixNano())
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
	x := Tools{Exec: func(_ context.Context, argv []string) (string, error) { return run(argv, nil) }}
	ctx := context.Background()
	for end := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		// pg_isready answers during the image's init restart; a query does not lie
		if _, err := run(pg{}.psql(i, "postgres", []string{"SELECT 1"}), nil); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("postgres never came up")
		}
	}

	src := Slice{Name: "br_src", User: "br_src", Password: "src-pw"}
	cp := Slice{Name: "br_copy", User: "br_copy", Password: "copy-pw"}
	for _, s := range []Slice{src, cp} {
		if err := (pg{}).Provision(ctx, i, s, x); err != nil {
			t.Fatal(err)
		}
	}
	// the source has a table and a row, made by its owner
	if _, err := run(pg{}.psql(i, src.Name, []string{
		`SET ROLE "br_src"`,
		`CREATE TABLE t (id serial PRIMARY KEY, v text)`,
		`INSERT INTO t (v) VALUES ('hello branch')`,
	}), nil); err != nil {
		t.Fatal(err)
	}

	// pg_dump in the "source container", straight into the load
	dump, err := run(pg{}.SliceDump(i, src), nil)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if f, _ := SniffDump([]byte(dump)); f != DumpCustom {
		t.Fatalf("dump sniffed as %q, want custom", f)
	}
	if _, err := run(pg{}.Seed(DumpCustom, i, cp), []byte(dump)); err != nil {
		t.Fatalf("load: %v", err)
	}

	got, err := run(append(pg{}.psql(i, cp.Name, nil), "-tA", "-c",
		`SELECT tableowner || ':' || (SELECT v FROM t) FROM pg_tables WHERE tablename = 't'`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := cp.User + ":hello branch"; strings.TrimSpace(got) != want {
		t.Errorf("branch owner:row = %q, want %q", got, want)
	}

	// over TCP: the owner reaches its own database, the branch's cred cannot reach
	// the source's (CONNECT is revoked from PUBLIC)
	conn := func(s Slice, db string) error {
		_, err := run([]string{"env", "PGPASSWORD=" + s.Password, "psql", "-h", "127.0.0.1",
			"-U", s.User, "-d", db, "-tA", "-c", "SELECT 1"}, nil)
		return err
	}
	if err := conn(src, src.Name); err != nil {
		t.Fatalf("the source's owner cannot connect to its own database: %v", err)
	}
	if err := conn(cp, cp.Name); err != nil {
		t.Fatalf("the branch's owner cannot connect to the branch: %v", err)
	}
	if err := conn(cp, src.Name); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the branch's cred reached the source database: %v", err)
	}
}
