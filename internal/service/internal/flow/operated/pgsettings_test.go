package operated

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestImageMajor(t *testing.T) {
	for image, want := range map[string]int{
		"postgres:17":                   17,
		"postgres:18-alpine":            18,
		"postgres:16.4":                 16,
		"registry:5000/postgres:15":     15,
		"postgres":                      0,
		"postgres:latest":               0,
		"postgres@sha256:abcdef":        0,
		"registry:5000/postgres":        0,
		"postgres:17@sha256:abcdef0123": 0,
	} {
		if got := ImageMajor(image); got != want {
			t.Errorf("ImageMajor(%q) = %d, want %d", image, got, want)
		}
	}
}

// 18 moved PGDATA and the volume; 14 to 17 keep the old path.
func TestPGDataPathPerMajor(t *testing.T) {
	for _, c := range []struct {
		major                  int
		mount, data, prefix, w string
	}{
		{17, "/var/lib/postgresql/data", "/var/lib/postgresql/data", "", "/var/lib/postgresql/data/pg_wal"},
		{14, "/var/lib/postgresql/data", "/var/lib/postgresql/data", "", "/var/lib/postgresql/data/pg_wal"},
		{0, "/var/lib/postgresql/data", "/var/lib/postgresql/data", "", "/var/lib/postgresql/data/pg_wal"},
		{18, "/var/lib/postgresql", "/var/lib/postgresql/18/docker", "18/docker", "/var/lib/postgresql/18/docker/pg_wal"},
	} {
		if pgMount(c.major) != c.mount || pgDataDir(c.major) != c.data || PGDataPrefix(c.major) != c.prefix || PGWalDir(c.major) != c.w {
			t.Errorf("major %d: %s %s %q %s", c.major, pgMount(c.major), pgDataDir(c.major), PGDataPrefix(c.major), PGWalDir(c.major))
		}
	}
	d := pg{}.Definition()
	if v := d.Volumes(18); !slices.Equal(v, []string{"/var/lib/postgresql"}) {
		t.Errorf("18 volumes = %v", v)
	}
	if v := d.Volumes(16); !slices.Equal(v, []string{"/var/lib/postgresql/data"}) {
		t.Errorf("16 volumes = %v", v)
	}
	// the restore paths follow the major too
	i := Instance{Major: 18}
	if conf := (pg{}).RecoveryConf(i, time.Time{}); !strings.Contains(conf, "cp /var/lib/postgresql/18/docker/"+PGRestoreWalDir+"/%f %p") {
		t.Errorf("18 restore_command: %s", conf)
	}
	if conf := (pg{}).RecoveryConf(Instance{Major: 16}, time.Time{}); !strings.Contains(conf, "cp /var/lib/postgresql/data/"+PGRestoreWalDir+"/%f %p") {
		t.Errorf("16 restore_command: %s", conf)
	}
	if s := strings.Join((pg{}).SettleArgv(i), " "); !strings.Contains(s, "/var/lib/postgresql/18/docker") || strings.Contains(s, "ALTER SYSTEM") {
		t.Errorf("settle: %s", s)
	}
}

// The owned flags: allow_alter_system exists from 17 only; archiving only
// with PITR; restore_command and the rest are reserved, never emitted.
func TestPGFlagsPerMajor(t *testing.T) {
	has := func(flags []string, name string) bool {
		return slices.ContainsFunc(flags, func(f string) bool { return strings.HasPrefix(f, name+"=") })
	}
	for _, c := range []struct {
		major, days int
		alter       bool
		archive     bool
	}{
		{16, 0, false, false},
		{15, 7, false, true},
		{17, 0, true, false},
		{18, 7, true, true},
		{0, 0, true, false},
	} {
		f := pgFlags(c.major, c.days, 0)
		if has(f, "allow_alter_system") != c.alter || has(f, "archive_mode") != c.archive {
			t.Errorf("major %d days %d: %v", c.major, c.days, f)
		}
		for _, always := range []string{"shared_preload_libraries", "wal_log_hints", "wal_level", "max_wal_senders", "max_replication_slots", "hot_standby"} {
			if !has(f, always) {
				t.Errorf("major %d: no %s in %v", c.major, always, f)
			}
		}
		for _, never := range []string{"restore_command", "hba_file", "primary_conninfo", "synchronous_standby_names"} {
			if has(f, never) {
				t.Errorf("major %d emits reserved %s", c.major, never)
			}
		}
	}
	if f := pgFlags(17, 0, 0); !slices.Contains(f, "shared_preload_libraries=pg_stat_statements") || !slices.Contains(f, "wal_log_hints=on") {
		t.Errorf("flags = %v", f)
	}
	cmd := pgCommand(Instance{Major: 16, PITRDays: 7})
	if cmd[0] != "sh" || !strings.Contains(cmd[2], "chown postgres:postgres "+PGArchiveDir) || !slices.Contains(cmd, "archive_mode=on") {
		t.Errorf("command = %v", cmd)
	}
	if cmd := pgCommand(Instance{Major: 16}); strings.Contains(cmd[2], PGArchiveDir) {
		t.Errorf("no archive volume, yet the command chowns it")
	}
}

// The scripts parse as shell; running them needs the image (make test-integration).
func TestScriptsParse(t *testing.T) {
	for name, script := range map[string]string{
		"start":  pgStartScript,
		"settle": pg{}.SettleArgv(Instance{})[2],
	} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s script: %v %s", name, err, out)
		}
	}
}

func TestOwnedNamesAreRefused(t *testing.T) {
	for _, n := range []string{
		"wal_level", "archive_mode", "archive_command", "archive_timeout", "restore_command", "listen_addresses", "port",
		"hba_file", "ssl", "ssl_cert_file", "shared_preload_libraries", "allow_alter_system", "wal_log_hints",
		"primary_conninfo", "synchronous_standby_names", "recovery_target_time", "include", "fsync", "full_page_writes", "wal_keep_size",
	} {
		if err := CheckSettings(map[string]string{n: "x"}); err == nil || !strings.Contains(err.Error(), "managed by stackr") {
			t.Errorf("%s: err = %v", n, err)
		}
	}
	for _, n := range append(slices.Clone(PGMain), PGAdvanced...) {
		if PGOwned(n) {
			t.Errorf("offered setting %s is owned", n)
		}
		if err := CheckSettings(map[string]string{n: "1"}); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	for _, v := range []string{"on", "remote_write", "remote_apply"} {
		if err := CheckSettings(map[string]string{"synchronous_commit": v}); err != nil {
			t.Errorf("synchronous_commit=%s: %v", v, err)
		}
	}
	for _, v := range []string{"off", "local", "false", "0"} {
		if CheckSettings(map[string]string{"synchronous_commit": v}) == nil {
			t.Errorf("synchronous_commit=%s passed", v)
		}
	}
	for _, bad := range []map[string]string{{"Bad Name": "1"}, {"x": ""}, {"x": "a\nb"}, {"1x": "1"}} {
		if CheckSettings(bad) == nil {
			t.Errorf("%v passed", bad)
		}
	}
}

func TestComputedDefaults(t *testing.T) {
	// no limit: the node is shared
	c := pgComputed(0, 8192)
	if c["shared_buffers"] != "256MB" || c["effective_cache_size"] != "4096MB" || c["maintenance_work_mem"] != "64MB" || c["random_page_cost"] != "1.1" {
		t.Errorf("no limit: %v", c)
	}
	if c := pgComputed(0, 0); c["effective_cache_size"] != "1024MB" {
		t.Errorf("unknown node: %v", c)
	}
	// a limit: the memory sets them
	c = pgComputed(4096, 65536)
	if c["shared_buffers"] != "1024MB" || c["effective_cache_size"] != "3072MB" || c["maintenance_work_mem"] != "204MB" || c["random_page_cost"] != "1.1" {
		t.Errorf("limit: %v", c)
	}
	// the user wins
	e := PGEffective(Instance{MemMB: 4096, Settings: map[string]string{"shared_buffers": "2GB", "max_connections": "50"}})
	if e["shared_buffers"] != "2GB" || e["max_connections"] != "50" || e["effective_cache_size"] != "3072MB" {
		t.Errorf("effective: %v", e)
	}
}

func TestStackrConfRendering(t *testing.T) {
	got := pgConf(Instance{NodeMemMB: 2048, Settings: map[string]string{"timezone": "Europe/Berlin", "log_line_prefix": `it's \ ok`}})
	want := `# written by stackr from its own database; edits here are overwritten
effective_cache_size = '1024MB'
log_line_prefix = 'it''s \\ ok'
maintenance_work_mem = '64MB'
random_page_cost = '1.1'
shared_buffers = '256MB'
timezone = 'Europe/Berlin'
`
	if got != want {
		t.Errorf("conf:\n%s\nwant:\n%s", got, want)
	}
	// the env that seeds it carries it
	env := pg{}.Definition().Config(Instance{AdminUser: "stackr", AdminDB: "postgres", NodeMemMB: 2048})
	if !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "STACKR_PG_CONF=# written by stackr") }) {
		t.Errorf("env = %v", env)
	}
}

func TestReloadOrRestart(t *testing.T) {
	ctxs := map[string]string{
		"work_mem": "user", "statement_timeout": "user", "autovacuum_vacuum_scale_factor": "sighup",
		"log_connections": "superuser-backend", "max_connections": "postmaster", "shared_buffers": "postmaster",
		"server_version_num": "internal",
	}
	for _, c := range []struct {
		changed, unset []string
		restart        bool
		err            bool
	}{
		{[]string{"work_mem", "statement_timeout"}, nil, false, false},
		{[]string{"work_mem", "autovacuum_vacuum_scale_factor", "log_connections"}, nil, false, false},
		{[]string{"work_mem", "max_connections"}, nil, true, false},
		{[]string{"shared_buffers"}, []string{"shared_buffers"}, true, false},
		{[]string{"nope"}, nil, false, true},
		{[]string{"nope"}, []string{"nope"}, false, false},
		{[]string{"server_version_num"}, nil, false, true},
	} {
		restart, err := pgJudge(ctxs, c.changed, c.unset)
		if restart != c.restart || (err != nil) != c.err {
			t.Errorf("%v: restart %v err %v, want %v %v", c.changed, restart, err, c.restart, c.err)
		}
	}
}

func TestChangedSettings(t *testing.T) {
	changed, unset := ChangedSettings(
		map[string]string{"a": "1", "b": "2", "c": "3"},
		map[string]string{"a": "1", "b": "9", "d": "4"},
	)
	if !slices.Equal(changed, []string{"b", "c", "d"}) || !slices.Equal(unset, []string{"c"}) {
		t.Errorf("changed %v unset %v", changed, unset)
	}
}

func TestWorkMemRisk(t *testing.T) {
	if !PGWorkMemRisk("64MB", "200", 8192) { // 12.8 GB > 8 GB
		t.Error("64MB x 200 on 8 GB is a risk")
	}
	if PGWorkMemRisk("4MB", "100", 8192) {
		t.Error("4MB x 100 on 8 GB is fine")
	}
	if PGWorkMemRisk("64MB", "200", 0) {
		t.Error("an unknown node is no verdict")
	}
	if !PGWorkMemRisk("1048576", "100", 50) { // bare = kB: 1 GB x 100
		t.Error("bare kB")
	}
}

// MO2: no password on any admin exec's argv or env, and none in the script.
// The admin and the slice's owner connect over the container's own socket;
// a role's password goes in on stdin.
func TestNoPasswordOnExecArgv(t *testing.T) {
	i := Instance{AdminUser: "stackr", AdminPassword: "ADMINPW", AdminDB: "postgres", PITRDays: 7}
	s := Slice{Name: "orders", User: "orders", Password: "SLICEPW"}
	argvs := [][]string{
		pg{}.psql(i, "postgres", []string{"SELECT 1"}),
		pg{}.Backup("dump", i),
		pg{}.Backup("base", i),
		pg{}.Restore("dump", "postgres", i),
		pg{}.WalStartArgv(i),
		pg{}.WalStopArgv(i),
		pg{}.InRecoveryArgv(i),
		pg{}.SettleArgv(i),
		pg{}.ArchiverArgv(i),
		pg{}.SliceDump(i, s),
		pg{}.Seed(DumpCustom, i, s),
		pg{}.Seed(DumpPlain, i, s),
		pgConfWrite(i, pgConf(i)),
	}
	for _, a := range argvs {
		j := strings.Join(a, " ")
		if strings.Contains(j, "ADMINPW") || strings.Contains(j, "SLICEPW") || strings.Contains(j, "PGPASSWORD") {
			t.Errorf("password in %q", j)
		}
	}
	argv, in := pgSQL(i, "postgres", []string{`CREATE ROLE "orders" LOGIN PASSWORD 'SLICEPW'`})
	if strings.Contains(strings.Join(argv, " "), "SLICEPW") || !strings.Contains(in, "SLICEPW") || !slices.Contains(argv, "-f") {
		t.Errorf("sql: argv %v in %q", argv, in)
	}
	// the container's env holds the admin password for the image's first boot
	// and nothing else is added to an exec
	if !slices.Contains(pg{}.Definition().Config(i), "POSTGRES_PASSWORD=ADMINPW") {
		t.Error("first boot lost its password")
	}
}
