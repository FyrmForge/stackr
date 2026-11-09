package operated

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func TestSniffDump(t *testing.T) {
	tar := make([]byte, 300)
	copy(tar[257:], "ustar")
	for _, c := range []struct {
		name string
		head []byte
		want string // "" = refused
	}{
		{"custom", []byte("PGDMP\x01\x0e\x00"), DumpCustom},
		{"plain", []byte("--\n-- PostgreSQL database dump\n"), DumpPlain},
		{"plain without a header", []byte("CREATE TABLE t (id int);"), DumpPlain},
		{"empty", nil, ""},
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, ""},
		{"tar", tar, ""},
	} {
		got, err := SniffDump(c.head)
		if got != c.want {
			t.Errorf("%s: format = %q, want %q", c.name, got, c.want)
		}
		if _, invalid := errs.IsInvalid(err); (c.want == "") != invalid {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestPgSeedArgv(t *testing.T) {
	s := Slice{Name: "shop_dev_db", User: "shop_dev_db", Password: "pw"}
	custom := pg{}.Seed(DumpCustom, Instance{AdminUser: "stackr"}, s)
	for _, want := range []string{"pg_restore", "--no-owner", "--no-privileges", "--exit-on-error", "--single-transaction"} {
		if !slices.Contains(custom, want) {
			t.Errorf("custom argv %q lacks %s", custom, want)
		}
	}
	// as the slice's owner, never the admin
	if i := slices.Index(custom, "-U"); i < 0 || custom[i+1] != s.User {
		t.Errorf("custom argv %q does not run as %s", custom, s.User)
	}
	plain := pg{}.Seed(DumpPlain, Instance{}, s)
	if plain[len(plain)-2] != s.User || plain[len(plain)-1] != s.Name || !strings.Contains(plain[len(plain)-3], "--single-transaction") {
		t.Errorf("plain argv = %q", plain)
	}
	if argv := (pg{}).Seed("tar", Instance{}, s); argv != nil {
		t.Error("an unknown format got an argv")
	}
}

// The plain filter drops owners, grants and the dump's own \restrict lines,
// never COPY rows that read the same.
func TestPgSeedFilter(t *testing.T) {
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk")
	}
	in := strings.Join([]string{
		`\restrict k1`,
		"CREATE TABLE public.t (v text);",
		"ALTER TABLE public.t OWNER TO alice;",
		"COPY public.t (v) FROM stdin;",
		"GRANT ALL ON t TO bob;",
		`\.`,
		"GRANT SELECT ON TABLE public.t TO bob;",
		"REVOKE ALL ON SCHEMA public FROM PUBLIC;",
		"ALTER DEFAULT PRIVILEGES FOR ROLE alice GRANT ALL ON TABLES TO bob;",
		"SET SESSION AUTHORIZATION alice;",
		"CREATE INDEX t_v ON public.t (v);",
		`\unrestrict k1`,
	}, "\n") + "\n"
	cmd := exec.Command(awk, pgSeedFilter)
	cmd.Stdin = strings.NewReader(in)
	cmd.Env = []string{"LC_ALL=C"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"CREATE TABLE public.t (v text);",
		"COPY public.t (v) FROM stdin;",
		"GRANT ALL ON t TO bob;",
		`\.`,
		"CREATE INDEX t_v ON public.t (v);",
	}, "\n") + "\n"
	if !bytes.Equal(out, []byte(want)) {
		t.Errorf("filtered =\n%s\nwant\n%s", out, want)
	}
}
