package operated

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The replication role is created, or altered when it exists (a replayed
// CREATE would make postgres log the statement, password included); the slot
// is created once; a slot name has no hyphens.
func TestReplicationSQL(t *testing.T) {
	if got := replRoleSQL(false, "pw")[0]; got != `CREATE ROLE stackr_repl REPLICATION LOGIN PASSWORD 'pw'` {
		t.Errorf("create = %s", got)
	}
	if got := replRoleSQL(true, "pw")[0]; !strings.HasPrefix(got, "ALTER ROLE stackr_repl REPLICATION") {
		t.Errorf("alter = %s", got)
	}
	slot := SlotName("0b7c-4f1e-aa")
	if slot != "stackr_0b7c_4f1e_aa" {
		t.Errorf("slot = %s", slot)
	}
	if c := slotCreateSQL(slot); !strings.Contains(c, "pg_create_physical_replication_slot('"+slot+"')") || !strings.Contains(c, "NOT EXISTS") {
		t.Errorf("create slot = %s", c)
	}
	d := strings.Join(slotDropSQL(slot), ";")
	if !strings.Contains(d, "pg_terminate_backend(active_pid)") || !strings.Contains(d, "pg_drop_replication_slot") {
		t.Errorf("drop slot = %s", d)
	}
	got := ParseSlotStates("stackr_a|reserved\nstackr_b|lost\nstackr_c|\n")
	if got["stackr_a"] != "reserved" || got["stackr_b"] != "lost" || len(got) != 3 {
		t.Errorf("slot states = %v", got)
	}
	if !StandbyStreaming("1,streaming\n") || StandbyStreaming("1,") || StandbyStreaming("0,") {
		t.Error("standby probe misread")
	}
}

// The owned settings split: postmaster ones are start flags on every member,
// the reloadable ones are in stackr-owned.conf, and users cannot set either.
func TestOwnedSplit(t *testing.T) {
	flags := pgFlags(17, 0, 0)
	for _, f := range []string{"wal_level=replica", "max_wal_senders=10", "max_replication_slots=10", "hot_standby=on", "wal_log_hints=on"} {
		if !slices.Contains(flags, f) {
			t.Errorf("no -c %s in %v", f, flags)
		}
	}
	for _, f := range flags {
		if strings.HasPrefix(f, "primary_conninfo") || strings.HasPrefix(f, "max_slot_wal_keep_size") {
			t.Errorf("a reloadable setting is a start flag: %s", f)
		}
	}
	primary := pgOwnedConf(Instance{SlotKeepMB: 20480, Port: 5432})
	if !strings.Contains(primary, "max_slot_wal_keep_size = '20480MB'") || strings.Contains(primary, "primary_conninfo") {
		t.Errorf("primary owned conf:\n%s", primary)
	}
	standby := pgOwnedConf(Instance{SlotKeepMB: 10240, Port: 5432, PrimaryHost: "db-1234abcd", Slot: "stackr_x", ReplPassword: "hunter2"})
	for _, want := range []string{
		"primary_conninfo = 'host=db-1234abcd port=5432 user=stackr_repl passfile=" + ReplPassfile + " application_name=stackr_x",
		"primary_slot_name = 'stackr_x'",
	} {
		if !strings.Contains(standby, want) {
			t.Errorf("standby owned conf lacks %q:\n%s", want, standby)
		}
	}
	if strings.Contains(standby, "hunter2") {
		t.Error("the replication password is in a config file")
	}
	// the conf is included after stackr.conf, and written before the include
	if i, j := strings.Index(pgStartScript, "include 'stackr.conf'"), strings.Index(pgStartScript, "include 'stackr-owned.conf'"); i < 0 || j < i {
		t.Error("stackr-owned.conf is not included after stackr.conf")
	}
	for _, n := range []string{"max_wal_senders", "max_replication_slots", "hot_standby", "max_slot_wal_keep_size", "primary_slot_name", "primary_conninfo"} {
		if err := CheckSettings(map[string]string{n: "1"}); err == nil || !strings.Contains(err.Error(), "managed by stackr") {
			t.Errorf("%s: err = %v", n, err)
		}
	}
}

// max_slot_wal_keep_size: the larger of 10 GB and 10% of the free disk, in
// whole GB, but never over the free disk less a headroom (10% of the disk, 5
// GB at least), and 1 GB at the least.
func TestSlotKeepMB(t *testing.T) {
	for _, c := range []struct{ free, total, want int }{
		{0, 0, 10240},                 // unknown
		{50 << 10, 100 << 10, 10240},  // 10 GB floor
		{250 << 10, 400 << 10, 25600}, // 10% of free
		{12 << 10, 100 << 10, 2048},   // free 12 GB less 10 GB headroom
		{6 << 10, 100 << 10, 1024},    // under the budget: the 1 GB floor
		{20 << 10, 20 << 10, 10240},   // headroom 5 GB: budget 15 GB, 10 GB wanted
	} {
		if got := slotKeepMB(c.free, c.total); got != c.want {
			t.Errorf("free %d total %d MB: keep %d, want %d", c.free, c.total, got, c.want)
		}
	}
}

// The standby's script parses, copies with the replication role from the
// passfile (never an argv), never with -R (that writes primary_conninfo to
// postgresql.auto.conf, which beats stackr-owned.conf), and stops instead of
// falling through to initdb when the copy fails.
func TestStandbyBootstrap(t *testing.T) {
	i := Instance{Major: 17, Port: 5432, PrimaryHost: "db-1234abcd", Slot: "stackr_x", ReplPassword: "hunter2", AdminUser: "stackr", AdminDB: "postgres"}
	cmd := standbyCommand(i)
	if out, err := exec.Command("sh", "-n", "-c", cmd[2]).CombinedOutput(); err != nil {
		t.Fatalf("standby script: %v %s", err, out)
	}
	script := cmd[2]
	for _, want := range []string{
		"pg_basebackup", "-X stream", `-S "$STACKR_SLOT"`, "-U " + ReplUser, "PGPASSFILE=" + ReplPassfile, "standby.signal",
		"exit 1",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("no %q in the standby script", want)
		}
	}
	if regexp.MustCompile(`pg_basebackup[^\n]*( -R\b|--write-recovery-conf)`).MatchString(script) {
		t.Error("pg_basebackup -R would put primary_conninfo in postgresql.auto.conf")
	}
	// the copy comes before the image's entrypoint, which would initdb an empty directory
	if strings.Index(script, "pg_basebackup") > strings.Index(script, "exec docker-entrypoint.sh") {
		t.Error("the entrypoint runs before the copy")
	}
	if strings.Contains(script, "STACKR_REPL_PASS") {
		t.Error("the script reads the password from the env")
	}
	if strings.Contains(strings.Join(cmd, " "), "hunter2") {
		t.Error("the replication password is on the command line")
	}
	if !slices.Contains(cmd, "wal_level=replica") || slices.Contains(cmd, "archive_mode=on") {
		t.Errorf("standby flags = %v", cmd)
	}
}

// The standby's bootstrap, run for real with a stand-in pg_basebackup: an
// empty directory is copied and marked complete; one with PG_VERSION but no
// marker (a copy cut short), or no standby.signal, is wiped and copied again;
// a complete one is left alone; a failed copy leaves nothing and fails.
func TestStandbyBootstrapRuns(t *testing.T) {
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("pg_basebackup", `echo run >> "$CALLS"
[ -n "$FAIL" ] && { echo x > "$PGDATA/partial"; exit 1; }
echo 17 > "$PGDATA/PG_VERSION"; echo conf > "$PGDATA/stackr.conf"`)
	write("chown", "exit 0")
	pass := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(pass, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(standbyPrep, ReplPassfile, pass)
	data := filepath.Join(t.TempDir(), "data")
	calls := filepath.Join(t.TempDir(), "calls")
	run := func(fail bool) error {
		c := exec.Command("sh", "-c", script)
		c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PGDATA="+data, "CALLS="+calls,
			"STACKR_PRIMARY=p", "STACKR_PORT=5432", "STACKR_SLOT=s")
		if fail {
			c.Env = append(c.Env, "FAIL=1")
		}
		return c.Run()
	}
	copies := func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "run")
	}
	has := func(n string) bool { _, err := os.Stat(filepath.Join(data, n)); return err == nil }

	if err := run(true); err == nil || has("partial") || has("PG_VERSION") {
		t.Fatalf("a failed copy: err %v, left partial %v", err, has("partial"))
	}
	if err := run(false); err != nil || copies() != 2 || !has(standbyDone) || !has("standby.signal") || has("stackr.conf") {
		t.Fatalf("first run: err %v copies %d", err, copies())
	}
	if err := run(false); err != nil || copies() != 2 {
		t.Fatalf("a complete directory was copied again: err %v copies %d", err, copies())
	}
	os.Remove(filepath.Join(data, standbyDone)) // a copy cut short: data there, no marker
	os.WriteFile(filepath.Join(data, "stale"), nil, 0o600)
	if err := run(false); err != nil || copies() != 3 || has("stale") || !has(standbyDone) {
		t.Fatalf("no marker: err %v copies %d", err, copies())
	}
	// a promoted directory (marker, no standby.signal) is a primary now: never emptied
	os.Remove(filepath.Join(data, "standby.signal"))
	if err := run(false); err == nil || copies() != 3 || !has("PG_VERSION") || has("standby.signal") {
		t.Fatalf("promoted directory: err %v copies %d", err, copies())
	}
	// without the marker (a rewind that failed) it is copied afresh
	os.Remove(filepath.Join(data, standbyDone))
	if err := run(false); err != nil || copies() != 4 || !has("standby.signal") {
		t.Fatalf("no marker, no signal: err %v copies %d", err, copies())
	}
}

// The sync names are in their own file, which the start scripts never rewrite
// once it exists and a standby's copy of the primary's never keeps.
func TestSyncConfFile(t *testing.T) {
	names := SyncStandbyNames([]string{"stackr_a"})
	if got := pgSyncConf(names); !strings.Contains(got, `synchronous_standby_names = 'ANY 1 ("stackr_a")'`) {
		t.Errorf("sync conf lacks the names:\n%s", got)
	}
	if strings.Contains(pgSyncConf(""), "synchronous_standby_names") {
		t.Error("an async sync conf has names")
	}
	if strings.Contains(pgOwnedConf(Instance{SlotKeepMB: 1024}), "synchronous_standby_names") {
		t.Error("the owned conf carries the names")
	}
	if !strings.Contains(pgStartTail, `[ -e "$d/stackr-sync.conf" ] ||`) || strings.Contains(pgStartTail, `> "$d/stackr-sync.conf.new"`) {
		t.Error("a start must create the sync conf only when it is missing")
	}
	if !strings.Contains(standbyPrep, "stackr-sync.conf") || !strings.Contains(rewindScript, "stackr-sync.conf") {
		t.Error("a standby's copy keeps the primary's sync conf")
	}
}
