package operated

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

func TestPGFlagsPreload(t *testing.T) {
	t.Parallel()
	want := "shared_preload_libraries=pg_stat_statements,vchord,auto_explain"
	if f := pgFlags(17, 0, 0, []string{"vchord", "auto_explain"}); !slices.Contains(f, want) {
		t.Errorf("flags = %v, want %s", f, want)
	}
	i := Instance{Major: 17, Preload: []string{"vchord"}}
	for name, cmd := range map[string][]string{"primary": pgCommand(i), "standby": standbyCommand(i)} {
		if !slices.Contains(cmd, "shared_preload_libraries=pg_stat_statements,vchord") {
			t.Errorf("%s command does not load the preload: %v", name, cmd)
		}
	}
}

// extFake answers the catalog read with rows, records every other call.
type extFake struct {
	rows     string // "R|0" and E| lines
	calls    []string
	in       []string // stdin of ExecIn calls
	execErr  error
	stdinErr error
}

func (e *extFake) tools() Tools {
	return Tools{
		Exec: func(_ context.Context, a []string) (string, error) {
			j := strings.Join(a, " ")
			e.calls = append(e.calls, j)
			if strings.Contains(j, "pg_available_extensions") {
				return e.rows, e.execErr
			}
			return "", e.execErr
		},
		ExecIn: func(_ context.Context, a []string, stdin string) (string, error) {
			e.calls = append(e.calls, strings.Join(a, " "))
			e.in = append(e.in, stdin)
			return "", e.stdinErr
		},
	}
}

var extInst = Instance{Slug: "pg", AdminUser: "stackr", AdminDB: "postgres", Major: 17}

const extRows = "R|0\nE|plpgsql|1.0|1.0|\nE|pg_stat_statements|1.10|1.11|\nE|vector||0.8.1|\nE|cube|1.5|1.5|earthdistance\nE|earthdistance||1.2|\n"

// Nothing to do is the one catalog read: no write session at all.
func TestReconcileNothingToDo(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: "R|0\nE|plpgsql|1.0|1.0|\nE|vector|0.8.1|0.8.1|\nE|other||2.0|\n"}
	if err := pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || len(f.in) != 0 {
		t.Errorf("calls = %v", f.calls)
	}
}

// Lock first, then the creates in list order, then the updates by name; the
// whole of it is one session.
func TestReconcileOrder(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: extRows}
	if err := pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector", "earthdistance", "vector"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.in) != 1 {
		t.Fatalf("sessions = %d", len(f.in))
	}
	want := []string{
		"SELECT pg_advisory_lock(7370303001)",
		`CREATE EXTENSION IF NOT EXISTS "vector" CASCADE`,
		`CREATE EXTENSION IF NOT EXISTS "earthdistance" CASCADE`,
		`ALTER EXTENSION "pg_stat_statements" UPDATE`,
	}
	if got := strings.Split(strings.TrimSuffix(f.in[0], ";\n"), ";\n"); !slices.Equal(got, want) {
		t.Errorf("statements = %q", got)
	}
}

// A standby is left alone, whatever the catalog says (R12).
func TestReconcileSkipsStandby(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: strings.Replace(extRows, "R|0", "R|1", 1)}
	if err := pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.in) != 0 {
		t.Errorf("a standby was written: %q", f.in)
	}
}

// Message 4: the image does not have it. Nothing is written.
func TestReconcileMissingFromImage(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: extRows}
	err := pgReconcile(context.Background(), extInst, f.tools(), "shop_dev_app", []string{"vchord"}, io.Discard)
	if err == nil || err.Error() != "slice shop_dev_app: the image of pg has no extension vchord." {
		t.Errorf("err = %v", err)
	}
	if len(f.in) != 0 {
		t.Error("something was written")
	}
	// a name that is not an extension name never reaches SQL
	if err := pgReconcile(context.Background(), extInst, f.tools(), "app", []string{`x"; DROP`}, io.Discard); err == nil || len(f.in) != 0 {
		t.Errorf("hostile name: %v", err)
	}
}

// Messages 8a and 8b: the failing statement is found from psql's line number.
func TestReconcileFailures(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: extRows, stdinErr: errors.New("psql:<stdin>:3: ERROR:  could not open extension control file\nDETAIL: x")}
	err := pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector", "earthdistance"}, io.Discard)
	if err == nil || err.Error() != "extension earthdistance was not created in app: ERROR:  could not open extension control file" {
		t.Errorf("8a: %v", err)
	}
	f = &extFake{rows: extRows, stdinErr: errors.New("psql:<stdin>:4: ERROR:  no update path")}
	err = pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector", "earthdistance"}, io.Discard)
	if err == nil || err.Error() != "extension pg_stat_statements in app was not updated to 1.11: ERROR:  no update path" {
		t.Errorf("8b: %v", err)
	}
	// a real exit carries the same text in stderr
	f = &extFake{rows: extRows, stdinErr: docker.ExitError{Code: 3, Stderr: "psql:<stdin>:2: ERROR:  boom"}}
	err = pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector"}, io.Discard)
	if err == nil || !strings.HasPrefix(err.Error(), "extension vector was not created in app: ") {
		t.Errorf("exit: %v", err)
	}
	// no line number: still said, with the database
	f = &extFake{rows: extRows, stdinErr: errors.New("connection lost")}
	if err = pgReconcile(context.Background(), extInst, f.tools(), "app", []string{"vector"}, io.Discard); err == nil || !strings.Contains(err.Error(), "app") {
		t.Errorf("unmapped: %v", err)
	}
}

// A catalog name that is not a plain name is skipped, never put in SQL.
func TestReconcileSkipsOddCatalogNames(t *testing.T) {
	t.Parallel()
	f := &extFake{rows: "R|0\nE|a\"b|1|2|\n"}
	if err := pgReconcile(context.Background(), extInst, f.tools(), "app", nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.in) != 0 {
		t.Errorf("wrote %q", f.in)
	}
}

func TestPGJudgeDotted(t *testing.T) {
	t.Parallel()
	ctxs := map[string]string{"work_mem": "user"}
	for _, c := range []struct {
		name string
		ext  bool
		err  bool
	}{
		{"vchordrq.probes", true, false},
		{"vchordrq.probes", false, true},
		{"nope", true, true},
	} {
		restart, err := pgJudge(ctxs, []string{c.name}, nil, c.ext)
		if restart || (err != nil) != c.err {
			t.Errorf("%s ext=%v: restart %v err %v", c.name, c.ext, restart, err)
		}
	}
}

func TestParseBootCheck(t *testing.T) {
	t.Parallel()
	out := "creating directory\nX\tplpgsql\t1.0\nX\tvector\t0.8.1\r\nX\tuuid-ossp\t1.1\nX\tbad name\t1\nnoise\n"
	got, err := ParseBootCheck(out)
	if err != nil || len(got) != 3 || got["vector"] != "0.8.1" || got["uuid-ossp"] != "1.1" || got["plpgsql"] != "1.0" {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := ParseBootCheck("noise\n"); err == nil {
		t.Error("no extensions was no error")
	}
}

// The script is a constant: names and settings ride in the environment only.
func TestBootCheckHoldsNoInput(t *testing.T) {
	t.Parallel()
	hostile := []string{"x$(id)", "a`b`", "q\"q", "n\nRUN y", "-rm"}
	b := BootCheck{Name: "bc-1", Image: "img:17", Major: 17, Preload: hostile, Conf: "a.b = 'x'\n$(id)"}
	sp := b.Spec()
	if sp.User != "postgres" || sp.Image != "img:17" || sp.Restart != "no" || sp.Labels[LabelPGBootCheck] != "bc-1" {
		t.Errorf("spec = %+v", sp)
	}
	if len(sp.Cmd) != 3 || sp.Cmd[0] != "sh" || sp.Cmd[1] != "-c" || sp.Cmd[2] != pgBootScript {
		t.Fatalf("cmd = %q", sp.Cmd)
	}
	for _, h := range append(hostile, "a.b", "pg_stat_statements") {
		if strings.Contains(pgBootScript, h) {
			t.Errorf("the script holds %q", h)
		}
	}
	env := strings.Join(sp.Env, "\n")
	for _, w := range []string{"HOME=/var/lib/postgresql", "shared_preload_libraries=pg_stat_statements,x$(id)", "CONF=a.b = 'x'"} {
		if !strings.Contains(env, w) {
			t.Errorf("no %q in env %q", w, env)
		}
	}
	// the rules of the spike: POSIX sh, no bash, no setpriv, no pipe into or out of psql
	for _, bad := range []string{"bash", "setpriv", "[[", "| psql"} {
		if strings.Contains(pgBootScript, bad) {
			t.Errorf("script has %q", bad)
		}
	}
	for _, need := range []string{"pg_config --bindir", "mktemp -d /tmp/bc.XXXXXX", `-k $D`, "listen_addresses=", "ON_ERROR_STOP=1"} {
		if !strings.Contains(pgBootScript, need) {
			t.Errorf("script lacks %q", need)
		}
	}
	if strings.Count(pgBootScript, "psql ") != 1 {
		t.Error("more than one psql call")
	}
}

// The script parses under every sh on the machine.
func TestBootScriptParses(t *testing.T) {
	t.Parallel()
	for _, sh := range []string{"sh", "dash", "busybox"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		args := []string{"-n", "-c", pgBootScript}
		if sh == "busybox" {
			args = []string{"sh", "-n", "-c", pgBootScript}
		}
		if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
			t.Errorf("%s: %v %s", sh, err, out)
		}
	}
}

// The plain-dump filter drops COMMENT ON EXTENSION outside COPY data only.
func TestSeedFilterDropsExtensionComment(t *testing.T) {
	t.Parallel()
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk")
	}
	in := "COMMENT ON EXTENSION vector IS 'x';\nCOMMENT ON TABLE t IS 'y';\nCOPY t (a) FROM stdin;\nCOMMENT ON EXTENSION vector IS 'row';\n\\.\nSELECT 1;\n"
	cmd := exec.Command(awk, pgSeedFilter)
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "COMMENT ON TABLE t IS 'y';\nCOPY t (a) FROM stdin;\nCOMMENT ON EXTENSION vector IS 'row';\n\\.\nSELECT 1;\n"
	if string(out) != want {
		t.Errorf("filtered = %q", out)
	}
}

// A bind provisions as before: the statements are the role, the database and
// pg_stat_statements, and no reconcile read.
func TestProvisionStatementsUnchanged(t *testing.T) {
	t.Parallel()
	var calls []string
	x := Tools{
		Exec: func(_ context.Context, a []string) (string, error) {
			calls = append(calls, strings.Join(a, " "))
			return "0,0", nil
		},
		ExecIn: func(_ context.Context, a []string, stdin string) (string, error) {
			calls = append(calls, stdin)
			return "", nil
		},
	}
	if err := (pg{}).Provision(context.Background(), extInst, Slice{Name: "d", User: "u", Password: "p"}, x); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, "\n")
	for _, w := range []string{`CREATE ROLE "u" LOGIN PASSWORD 'p'`, `CREATE DATABASE "d" OWNER "u"`, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"} {
		if !strings.Contains(all, w) {
			t.Errorf("no %q in\n%s", w, all)
		}
	}
	if strings.Contains(all, "pg_available_extensions") || strings.Contains(all, "advisory") {
		t.Errorf("provision reconciles:\n%s", all)
	}
}

// A major change keeps its old rule: an update that fails is a warning, a
// listed name that cannot be created still fails.
func TestReconcileWarnUpdates(t *testing.T) {
	t.Parallel()
	var log strings.Builder
	f := &extFake{rows: extRows, stdinErr: errors.New("psql:<stdin>:2: ERROR:  no update path")}
	if err := pgReconcileAs(context.Background(), extInst, f.tools(), "app", nil, &log, true); err != nil {
		t.Fatalf("update failure failed the step: %v", err)
	}
	if !strings.Contains(log.String(), "warning: extension pg_stat_statements in app was not updated") {
		t.Errorf("log = %q", log.String())
	}
	f = &extFake{rows: extRows, stdinErr: errors.New("psql:<stdin>:2: ERROR:  could not open extension control file")}
	err := pgReconcileAs(context.Background(), extInst, f.tools(), "app", []string{"vector"}, io.Discard, true)
	if err == nil || !strings.HasPrefix(err.Error(), "extension vector was not created in app: ") {
		t.Errorf("create: %v", err)
	}
}
