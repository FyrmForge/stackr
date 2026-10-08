package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseDotenv(t *testing.T) {
	got, bad := parseDotenv(strings.Join([]string{
		"# comment",
		"",
		"PLAIN=one",
		"export EXPORTED = two ",
		`DQ="a b # not a comment" # trailing`,
		`ESC="x\ny\"z"`,
		"SQ='$HOME \\n'",
		"EMPTY=",
		"INLINE=val # comment",
		"HASH=a#b",
		"Mixed_Case9=m",
		"BAD-NAME=x",
		"dotted.name=x",
		"NOEQUALS",
		`OPEN="never closed`,
		"PLAIN=again",
	}, "\n"))
	want := []dotenvEntry{
		{"plain", "again"},
		{"exported", "two"},
		{"dq", "a b # not a comment"},
		{"esc", "x\ny\"z"},
		{"sq", `$HOME \n`},
		{"empty", ""},
		{"inline", "val"},
		{"hash", "a#b"},
		{"mixed_case9", "m"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if strings.Join(bad, ",") != "BAD-NAME,dotted.name,line 14,OPEN" {
		t.Errorf("refused = %v", bad)
	}
}

func TestParamsImportSendsOneCall(t *testing.T) {
	r := fake(t, map[string]string{"/params": `[{"env":"dev","tile":"web","job":"j1"}]`})
	f := envFile(t, "DB_URL=postgres://x\nTOKEN=\"s3cret value\"\n", 0o600)
	code, out, errw := cli(t, "params", "import", f, "--scope", "env", "--stack", "shop", "--env", "dev", "--collection", "app", "--secret")
	if code != 0 {
		t.Fatalf("import = %d %q %q", code, out, errw)
	}
	w := writes(r)
	if len(w) != 1 || !strings.HasPrefix(w[0], "PATCH /api/v1/orgs/acme/stacks/shop/envs/dev/params ") {
		t.Fatalf("writes = %v", w)
	}
	for _, s := range []string{
		`{"collection":"app","kind":"secret","name":"db_url","value":"postgres://x"}`,
		`{"collection":"app","kind":"secret","name":"token","value":"s3cret value"}`,
	} {
		if !strings.Contains(w[0], s) {
			t.Errorf("body %q lacks %s", w[0], s)
		}
	}
	if !strings.Contains(errw, "redeploying web (dev): stackr job log j1 --follow") {
		t.Errorf("stderr %q, want the redeploy named", errw)
	}
	if strings.Contains(out+errw, "s3cret") || strings.Contains(out+errw, "postgres://") {
		t.Errorf("a value was echoed: %q %q", out, errw)
	}
}

func TestParamsImportDryRunNamesOnly(t *testing.T) {
	r := fake(t, nil)
	f := envFile(t, "DB_URL=postgres://x\nTOKEN=s3cret\n", 0o600)
	code, out, errw := cli(t, "params", "import", f, "--scope", "org", "--collection", "app", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run = %d %q", code, errw)
	}
	if !strings.Contains(out, "app.db_url") || !strings.Contains(out, "app.token") {
		t.Errorf("names missing: %q", out)
	}
	if strings.Contains(out+errw, "s3cret") || strings.Contains(out+errw, "postgres") {
		t.Errorf("a value was echoed: %q %q", out, errw)
	}
	if w := writes(r); len(w) != 0 {
		t.Errorf("dry run wrote %v", w)
	}
}

func TestParamsImportRefusesUnmappableNames(t *testing.T) {
	r := fake(t, nil)
	f := envFile(t, "GOOD=1\nBAD-NAME=topsecret\n", 0o600)
	code, out, errw := cli(t, "params", "import", f, "--scope", "org", "--collection", "app")
	if code == 0 || !strings.Contains(errw, "BAD-NAME") {
		t.Errorf("import = %d %q, want BAD-NAME listed", code, errw)
	}
	if strings.Contains(out+errw, "topsecret") || len(writes(r)) != 0 {
		t.Errorf("leaked or wrote: %q %q %v", out, errw, writes(r))
	}
}

func TestParamsImportWorldReadableWarnsAndImports(t *testing.T) {
	r := fake(t, nil)
	f := envFile(t, "A=1\n", 0o644)
	code, _, errw := cli(t, "params", "import", f, "--scope", "org", "--collection", "app")
	if code != 0 || !strings.Contains(errw, "warning") || len(writes(r)) != 1 {
		t.Errorf("import = %d %q writes %v, want a warning and one write", code, errw, writes(r))
	}
}

func TestParamsImportUnreadableFails(t *testing.T) {
	r := fake(t, nil)
	code, _, errw := cli(t, "params", "import", filepath.Join(t.TempDir(), "nope"), "--scope", "org", "--collection", "app")
	if code == 0 || len(writes(r)) != 0 {
		t.Errorf("missing file = %d %q writes %v, want an error and no write", code, errw, writes(r))
	}
}
