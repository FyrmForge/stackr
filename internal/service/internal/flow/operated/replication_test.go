package operated

import (
	"os/exec"
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
	flags := pgFlags(17, 0)
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

// max_slot_wal_keep_size: 10 GB, or 10% of the free disk in whole 10 GB steps.
func TestSlotKeepMB(t *testing.T) {
	for free, want := range map[int]int{0: 10240, 50 << 10: 10240, 150 << 10: 10240, 250 << 10: 20480, 1 << 20: 102400} {
		if got := slotKeepMB(free); got != want {
			t.Errorf("free %d MB: keep %d, want %d", free, got, want)
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
		`"$STACKR_REPL_PASS"`, "exit 1",
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
	if strings.Contains(strings.Join(cmd, " "), "hunter2") {
		t.Error("the replication password is on the command line")
	}
	if !slices.Contains(cmd, "wal_level=replica") || slices.Contains(cmd, "archive_mode=on") {
		t.Errorf("standby flags = %v", cmd)
	}
}
