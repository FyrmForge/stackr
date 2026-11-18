//go:build integration

// A writer on a slice may ALTER DATABASE ... SET; after a switch to read or
// an unbind it may not. Real postgres, oldest and newest supported majors.
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

func TestWriterIsMemberOfSliceOwner(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	for _, major := range []string{"14", "18"} {
		t.Run("pg"+major, func(t *testing.T) { writerOwnerCheck(t, major) })
	}
}

func writerOwnerCheck(t *testing.T, major string) {
	name := fmt.Sprintf("stkr-it-owner-%s-%d", major, time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "POSTGRES_USER=stackr", "-e", "POSTGRES_PASSWORD=admin", "postgres:"+major).CombinedOutput(); err != nil {
		t.Skipf("no postgres:%s container: %v %s", major, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	i := Instance{AdminUser: "stackr", AdminPassword: "admin", AdminDB: "postgres"}
	run := func(argv []string, in ...string) (string, error) {
		cmd := exec.Command("docker", append([]string{"exec", "-i", name}, argv...)...)
		cmd.Stdin = strings.NewReader(strings.Join(in, ""))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.String(), fmt.Errorf("%w: %s", err, errb.String())
		}
		return out.String(), nil
	}
	x := Tools{
		Exec:   func(_ context.Context, argv []string) (string, error) { return run(argv) },
		ExecIn: func(_ context.Context, argv []string, in string) (string, error) { return run(argv, in) },
	}
	ctx := context.Background()
	for end := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		if _, err := run([]string{"env", "PGPASSWORD=admin", "psql", "-h", "127.0.0.1", "-U", "stackr", "-d", "postgres", "-c", "SELECT 1"}); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("postgres never came up")
		}
	}

	s := Slice{Name: "own_db", User: "own_db", Password: "own-pw"}
	app := Grant{User: "own_app", Password: "app-pw", Access: "write"}
	if err := (pg{}).Provision(ctx, i, s, x); err != nil {
		t.Fatal(err)
	}
	// as the app, over TCP: does ALTER DATABASE ... SET work
	alter := func() error {
		_, err := run([]string{"env", "PGPASSWORD=" + app.Password, "psql", "-h", "127.0.0.1",
			"-U", app.User, "-d", s.Name, "-v", "ON_ERROR_STOP=1", "-c",
			`ALTER DATABASE "own_db" SET search_path = public`})
		return err
	}
	if err := (pg{}).Bind(ctx, i, s, app, "", nil, x); err != nil {
		t.Fatal(err)
	}
	if err := alter(); err != nil {
		t.Fatalf("a writer cannot ALTER DATABASE SET: %v", err)
	}
	// binding again is safe
	if err := (pg{}).Bind(ctx, i, s, app, "write", nil, x); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if err := alter(); err != nil {
		t.Fatalf("after rebind: %v", err)
	}
	// switched to read
	app.Access = "read"
	if err := (pg{}).Bind(ctx, i, s, app, "write", nil, x); err != nil {
		t.Fatal(err)
	}
	if err := alter(); err == nil || !strings.Contains(err.Error(), "must be owner") {
		t.Errorf("a reader altered the database: %v", err)
	}
	// back to write, then unbind
	app.Access = "write"
	if err := (pg{}).Bind(ctx, i, s, app, "read", nil, x); err != nil {
		t.Fatal(err)
	}
	if err := alter(); err != nil {
		t.Fatalf("read to write: %v", err)
	}
	if err := (pg{}).Unbind(ctx, i, s, app, x); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(append(pg{}.psql(i, s.Name, nil), "-tA", "-c",
		`SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = 'own_app'`)); strings.TrimSpace(out) != "0" {
		t.Errorf("memberships left after unbind: %q", out)
	}
}
