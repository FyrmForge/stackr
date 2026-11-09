package operated

import (
	"strings"
	"testing"
)

func poolerInst(settings map[string]string) Instance {
	return Instance{
		Slug: "db", AdminUser: "stackr", AdminDB: "postgres", Host: "db-1234abcd", Port: 5432,
		Major: 17, Settings: settings, PoolerPassword: "pw",
	}
}

func TestPoolerIni(t *testing.T) {
	ini, err := poolerIni(poolerInst(nil), []string{"b_db", "a_db", "a_db"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pool_mode = transaction",
		"max_prepared_statements = 200",
		"ignore_startup_parameters = extra_float_digits,options",
		"listen_port = 6432",
		"unix_socket_dir = /var/run/pgbouncer",
		"auth_user = stackr_pooler",
		"auth_dbname = postgres",
		"auth_query = SELECT usename, passwd FROM stackr_pooler.get_auth($1)",
		"a_db = host=db-1234abcd port=5432 pool_size=20 max_db_connections=44",
		"b_db = host=db-1234abcd port=5432 pool_size=20 max_db_connections=44",
		"postgres = host=db-1234abcd port=5432 pool_size=2 max_db_connections=2",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("no %q in\n%s", want, ini)
		}
	}
	if strings.Count(ini, "a_db =") != 1 {
		t.Errorf("a database listed twice:\n%s", ini)
	}
	if strings.Contains(ini, "pw") {
		t.Errorf("the ini carries the password")
	}
	if strings.Index(ini, "a_db =") > strings.Index(ini, "b_db =") {
		t.Errorf("databases not sorted")
	}
}

// The pools together never exceed max_connections less the headroom; a
// budget that cannot give every database one connection is refused.
func TestPoolCaps(t *testing.T) {
	for _, c := range []struct{ maxConn, n int }{{100, 1}, {100, 3}, {100, 7}, {500, 12}, {30, 2}, {200, 0}, {100, 88}} {
		auth, per, err := poolCaps(c.maxConn, c.n)
		if err != nil {
			t.Errorf("max_connections %d, %d dbs: %v", c.maxConn, c.n, err)
			continue
		}
		if total := auth + per*c.n; total > c.maxConn-poolerHeadroom {
			t.Errorf("max_connections %d, %d dbs: pools hold %d", c.maxConn, c.n, total)
		}
		if c.n > 0 && per < 1 {
			t.Errorf("max_connections %d, %d dbs: per = %d", c.maxConn, c.n, per)
		}
	}
	// one database too many for the budget: refused, never squeezed in
	for _, c := range []struct{ maxConn, n int }{{100, 89}, {15, 9}, {10, 1}, {5, 1}} {
		if _, _, err := poolCaps(c.maxConn, c.n); err == nil {
			t.Errorf("max_connections %d, %d dbs: no error", c.maxConn, c.n)
		}
	}
	if _, err := poolerIni(poolerInst(map[string]string{"max_connections": "15"}), []string{"a", "b", "c", "d"}); err == nil {
		t.Errorf("an ini was rendered over the budget")
	}
	// max_connections comes from the effective settings, default 100
	if n := maxConnections(poolerInst(nil)); n != 100 {
		t.Errorf("default = %d", n)
	}
	small := poolerInst(map[string]string{"max_connections": "40"})
	if n := maxConnections(small); n != 40 {
		t.Errorf("override = %d", n)
	}
	ini, err := poolerIni(small, []string{"a_db"})
	if err != nil || !strings.Contains(ini, "a_db = host=db-1234abcd port=5432 pool_size=20 max_db_connections=28") {
		t.Errorf("a changed max_connections did not reach the ini (%v):\n%s", err, ini)
	}
}

// Every statement can run again: the role is created or altered by what
// exists, the function replaced, the grants repeatable; nothing but the
// pooler role may execute the lookup.
func TestPoolerAuthSQLRepeats(t *testing.T) {
	first, again := poolerAuthSQL(false, "pw"), poolerAuthSQL(true, "pw")
	if !strings.HasPrefix(first[0], "CREATE ROLE stackr_pooler LOGIN PASSWORD") || !strings.HasPrefix(again[0], "ALTER ROLE stackr_pooler LOGIN PASSWORD") {
		t.Errorf("role: %q / %q", first[0], again[0])
	}
	for i, st := range first[1:] {
		if st != again[i+1] {
			t.Errorf("statement %d differs between runs", i+1)
		}
		if strings.HasPrefix(st, "CREATE ") && !strings.HasPrefix(st, "CREATE SCHEMA IF NOT EXISTS") && !strings.HasPrefix(st, "CREATE OR REPLACE FUNCTION") {
			t.Errorf("not repeatable: %s", st)
		}
	}
	all := strings.Join(first, "\n")
	for _, want := range []string{
		"SECURITY DEFINER", "SET search_path = pg_catalog",
		"NOT rolsuper", "NOT rolreplication", "NOT rolbypassrls", "rolname <> 'stackr_pooler'",
		"CREATE ROLE stackr_pooler_probe LOGIN NOSUPERUSER",
		"REVOKE ALL ON FUNCTION stackr_pooler.get_auth(text) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION stackr_pooler.get_auth(text) TO stackr_pooler",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q", want)
		}
	}
}

func TestPoolerSpec(t *testing.T) {
	i := poolerInst(nil)
	s := poolerSpec(i, "INI", "stackr-operated-x")
	if s.User != "pgbouncer" || s.User == "" || s.User == "root" || s.User == "0" {
		t.Errorf("user = %q, want the non-root pgbouncer user", s.User)
	}
	if s.Restart != "unless-stopped" || len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" || s.Privileged {
		t.Errorf("restart %q, cap drop %v, privileged %v", s.Restart, s.CapDrop, s.Privileged)
	}
	if len(s.Networks) != 1 || s.Networks[0].Name != "stackr-operated-x" || s.Networks[0].Aliases[0] != "db-1234abcd-pooler" {
		t.Errorf("networks = %+v", s.Networks)
	}
	if len(s.Ports) != 0 || s.HostNetwork {
		t.Errorf("the pooler publishes a port or joins the host network")
	}
	if s.Image != PoolerImage || s.HealthCmd == "" {
		t.Errorf("image %q, health %q", s.Image, s.HealthCmd)
	}
	for _, a := range s.Cmd {
		if strings.Contains(a, "pw") && a != s.Cmd[2] {
			t.Errorf("the password is on the argv")
		}
	}
	if strings.Contains(strings.Join(s.Cmd, " "), `"pw"`) {
		t.Errorf("the password is in the start script")
	}
}

func TestPoolerReloadAndConsole(t *testing.T) {
	r := poolerReload("INI")
	if r[len(r)-1] != "INI" || !strings.Contains(r[2], "kill -HUP 1") || !strings.Contains(r[2], "mv ") {
		t.Errorf("reload = %v", r)
	}
	c := poolerConsole("SHOW POOLS")
	if strings.Contains(strings.Join(c, " "), "pw") || !strings.Contains(c[2], `$STACKR_PGB_PASS`) {
		t.Errorf("console = %v", c)
	}
	if a, w := parsePools("pgbouncer|pgbouncer|1|0|0|0\na_db|u|3|2|0|0\nb_db|u|1|1|0|0\n"); a != 4 || w != 3 {
		t.Errorf("pools = %d, %d", a, w)
	}
}

func TestReservedConnectionsPerMajor(t *testing.T) {
	has := func(f []string, s string) bool {
		for _, x := range f {
			if x == s {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		major int
		want  string
		not   string
	}{
		{14, "superuser_reserved_connections=5", "reserved_connections=5"},
		{15, "superuser_reserved_connections=5", "reserved_connections=5"},
		{16, "reserved_connections=5", "superuser_reserved_connections=5"},
		{17, "reserved_connections=5", "superuser_reserved_connections=5"},
		{18, "reserved_connections=5", "superuser_reserved_connections=5"},
		{0, "reserved_connections=5", "superuser_reserved_connections=5"},
	} {
		f := pgFlags(c.major, 0, 0)
		if !has(f, c.want) || has(f, c.not) {
			t.Errorf("major %d: %v", c.major, f)
		}
	}
	for _, n := range []string{"reserved_connections", "superuser_reserved_connections"} {
		if !PGOwned(n) {
			t.Errorf("%s is not owned", n)
		}
	}
}

func TestSliceOutputs(t *testing.T) {
	i := poolerInst(nil)
	outs := func(pooled bool) map[string]Binding {
		got := map[string]Binding{}
		for _, b := range (pg{}).Bindings(i, Slice{Name: "orders", User: "u", Password: "p", Pooled: pooled}) {
			got[b.Name] = b
		}
		return got
	}
	direct, pooled := outs(false), outs(true)
	const dURL, pURL = "postgres://u:p@db-1234abcd:5432/orders", "postgres://u:p@db-1234abcd-pooler:6432/orders"
	for name, want := range map[string]string{
		"url_direct": dURL, "url_pooled": pURL, "host_direct": "db-1234abcd", "host_pooled": "db-1234abcd-pooler",
		"port_direct": "5432", "port_pooled": "6432", "database": "orders", "user": "u", "password": "p",
	} {
		for _, got := range []map[string]Binding{direct, pooled} {
			if got[name].Value != want {
				t.Errorf("%s = %q, want %q", name, got[name].Value, want)
			}
		}
	}
	// the _read ones dial the read alias, direct: a standby has no pooler yet
	const rURL = "postgres://u:p@db-1234abcd-read:5432/orders"
	for k, want := range map[string]string{
		"url_read": rURL, "url_read_direct": rURL, "url_read_pooled": rURL, "host_read": "db-1234abcd-read",
		"host_read_direct": "db-1234abcd-read", "host_read_pooled": "db-1234abcd-read",
		"port_read": "5432", "port_read_direct": "5432", "port_read_pooled": "5432",
	} {
		for _, got := range []map[string]Binding{direct, pooled} {
			if got[k].Value != want {
				t.Errorf("%s = %q, want %q", k, got[k].Value, want)
			}
		}
	}
	// url, host, port follow the slice's setting
	for _, c := range []struct {
		got             map[string]Binding
		url, host, port string
	}{
		{direct, dURL, "db-1234abcd", "5432"},
		{pooled, pURL, "db-1234abcd-pooler", "6432"},
	} {
		for k, want := range map[string]string{"url": c.url, "host": c.host, "port": c.port} {
			if c.got[k].Value != want {
				t.Errorf("%s = %q, want %q", k, c.got[k].Value, want)
			}
		}
	}
	for _, k := range []string{"url", "url_direct", "url_pooled", "url_read", "url_read_direct", "url_read_pooled", "password"} {
		if !direct[k].Secret {
			t.Errorf("%s is not secret", k)
		}
	}
	for _, k := range []string{"host", "host_pooled", "port", "database", "user", "host_read"} {
		if direct[k].Secret {
			t.Errorf("%s is secret", k)
		}
	}
	for _, old := range []string{"DATABASE_URL", "DATABASE_URL_POOLED", "PGHOST", "PGPORT", "PGHOST_POOLED", "PGUSER", "PGPASSWORD", "PGDATABASE"} {
		if _, ok := direct[old]; ok {
			t.Errorf("old name %s still published", old)
		}
	}
}
