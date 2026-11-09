package operated_test

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// The dump streams into the instance as the slice's owner, through
// pg_restore for a custom dump; a failed load is the tail of its stderr.
func TestSeedLoadsIntoTheSlice(t *testing.T) {
	w := setup(t, "postgres")
	if err := w.f.Seed(ctx, w.orders, mflow.DumpCustom, strings.NewReader("PGDMP"), io.Discard); err == nil {
		t.Error("seed of an unprovisioned slice passed")
	} else if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("unprovisioned = %v, want a conflict", err)
	}
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)

	dump := "PGDMP\x01 the rest"
	must(t, w.f.Seed(ctx, w.orders, mflow.DumpCustom, strings.NewReader(dump), io.Discard))
	if string(w.fake.ExecIn) != dump {
		t.Errorf("stdin = %q, want the dump", w.fake.ExecIn)
	}
	var argv []string
	for _, c := range w.fake.Calls() {
		if c.Method == "ExecStream" {
			argv = c.Args
		}
	}
	if len(argv) == 0 || argv[0] != "c1" || !slices.Contains(argv, "pg_restore") {
		t.Fatalf("exec = %q, want pg_restore in c1", argv)
	}
	if i := slices.Index(argv, "-U"); i < 0 || argv[i+1] != p.DBUser || argv[i+3] != p.DBName {
		t.Errorf("exec = %q, want it as %s on %s", argv, p.DBUser, p.DBName)
	}

	w.fake.Err = map[string]error{"ExecStream": docker.ExitError{
		Code:   1,
		Stderr: strings.Repeat("noise\n", 50) + `pg_restore: error: relation "t" already exists`,
	}}
	err = w.f.Seed(ctx, w.orders, mflow.DumpPlain, strings.NewReader("CREATE TABLE t ();"), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `relation "t" already exists`) || strings.Count(err.Error(), "noise") > 19 {
		t.Errorf("failed load = %v, want the stderr tail", err)
	}
}

// A slice is dumped in its instance's container as the admin, without a
// password in argv, and the stream is what Seed loads; a failed dump says
// why.
func TestDumpSliceStreamsFromTheInstance(t *testing.T) {
	w := setup(t, "postgres")
	if _, _, err := w.f.DumpSlice(ctx, w.orders); err == nil {
		t.Error("dump of an unprovisioned slice passed")
	}
	p, err := w.f.Provision(ctx, w.db, w.orders)
	must(t, err)

	w.fake.ExecOut = "PGDMP-bytes"
	out, wait, err := w.f.DumpSlice(ctx, w.orders)
	must(t, err)
	got, err := io.ReadAll(out)
	must(t, err)
	must(t, wait())
	if string(got) != "PGDMP-bytes" {
		t.Errorf("stream = %q", got)
	}
	var argv []string
	for _, c := range w.fake.Calls() {
		if c.Method == "ExecStream" {
			argv = c.Args
		}
	}
	if len(argv) == 0 || argv[0] != "c1" || !slices.Contains(argv, "pg_dump") || !slices.Contains(argv, "-Fc") {
		t.Fatalf("exec = %q, want pg_dump -Fc in c1", argv)
	}
	if i := slices.Index(argv, "-d"); i < 0 || argv[i+1] != p.DBName {
		t.Errorf("exec = %q, want the slice's database %s", argv, p.DBName)
	}
	if strings.Contains(strings.Join(argv, " "), "PGPASSWORD") || strings.Contains(strings.Join(argv, " "), p.DBPassword) {
		t.Errorf("exec = %q carries a password", argv)
	}

	w.fake.Err = map[string]error{"ExecStream": docker.ExitError{Code: 1, Stderr: "pg_dump: error: connection failed"}}
	if _, _, err := w.f.DumpSlice(ctx, w.orders); err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Errorf("failed dump = %v, want pg_dump's stderr", err)
	}
}
