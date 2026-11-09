package operated_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The standby container: the primary's image and major, the data volume at
// the major's path, the instance network with the read alias, the hosts line,
// the member label, restart "no"; no password is in the env (a passfile is
// copied in).
func TestStandbySpec(t *testing.T) {
	w := setup(t, "postgres")
	slot := mflow.SlotName("m1")
	must(t, w.f.PrepareStandby(ctx, w.db, slot, nil))
	mem := store.OperatedMember{ID: "m1", Container: "stackr-member-m1", Volume: "stackr-member-m1-data"}
	spec, err := w.f.StandbySpec(ctx, w.db, mem, "db-x:10.208.1.2")
	must(t, err)
	m, err := w.f.Instances.GetByTile(ctx, w.db.ID)
	must(t, err)
	if m.ReplPassword == "" {
		t.Fatal("no replication password stored")
	}
	host := "db-" + w.instance.ID[:8]
	if spec.Image != "postgres:17" || spec.Restart != "no" || spec.Name != mem.Container ||
		!slices.Equal(spec.Volumes, []string{mem.Volume + ":/var/lib/postgresql/data"}) ||
		!slices.Equal(spec.ExtraHosts, []string{"db-x:10.208.1.2"}) || spec.Labels["stackr.member"] != "m1" ||
		!slices.EqualFunc(spec.Networks, []docker.NetAttach{{Name: operated.Network(w.instance.ID), Aliases: []string{host + "-read"}}}, sameNet) {
		t.Errorf("spec = %+v", spec)
	}
	for _, want := range []string{"STACKR_PRIMARY=" + host, "STACKR_SLOT=" + slot, "STACKR_PG_REWRITE=1"} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("no %s in the env", strings.SplitN(want, "=", 2)[0])
		}
	}
	// no secret in what docker inspect shows: the passfile is copied in
	for _, e := range spec.Env {
		if strings.Contains(e, m.ReplPassword) || strings.HasPrefix(e, "POSTGRES_PASSWORD=") || strings.Contains(e, m.AdminPassword) {
			t.Errorf("a secret is in the env: %s", strings.SplitN(e, "=", 2)[0])
		}
	}
	if spec.Labels[mflow.LabelConf] == "" {
		t.Error("no settings hash label")
	}
	pf, err := w.f.StandbyPassfile(ctx, w.db)
	must(t, err)
	if string(pf) != host+":5432:*:stackr_repl:"+m.ReplPassword+"\n" {
		t.Errorf("passfile = %q", pf)
	}
	if !slices.ContainsFunc(spec.Env, func(e string) bool {
		return strings.HasPrefix(e, "STACKR_PG_OWNED=") && strings.Contains(e, "primary_conninfo")
	}) {
		t.Error("the standby's owned conf has no primary_conninfo")
	}
	if strings.Contains(strings.Join(spec.Cmd, " "), m.ReplPassword) {
		t.Error("the replication password is on the command line")
	}
}

// Setting a standby up on the primary: the role and the slot are made over
// stdin (the password never rides an argv), pg_hba and the owned conf go to
// the running primary and are reloaded.
func TestPrepareStandby(t *testing.T) {
	w := setup(t, "postgres")
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m1", []string{"10.208.1.0/24"}))
	m, err := w.f.Instances.GetByTile(ctx, w.db.ID)
	must(t, err)
	var stdin, argv strings.Builder
	for _, c := range w.fake.Calls() {
		stdin.WriteString(c.Stdin)
		argv.WriteString(strings.Join(c.Args, " ") + "\n")
	}
	contains(t, "stdin", stdin.String(), "CREATE ROLE stackr_repl REPLICATION LOGIN PASSWORD '"+m.ReplPassword+"'", "pg_create_physical_replication_slot('stackr_m1')")
	contains(t, "argv", argv.String(), "stackr-owned.conf", "host replication stackr_repl 10.208.1.0/24 scram-sha-256", "pg_reload_conf()")
	if strings.Contains(argv.String(), m.ReplPassword) {
		t.Error("the replication password is on an argv")
	}
	// a second standby re-syncs: the password stays
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m2", []string{"10.208.1.0/24", "10.208.2.0/24"}))
	if m2, _ := w.f.Instances.GetByTile(ctx, w.db.ID); m2.ReplPassword != m.ReplPassword {
		t.Error("the replication password changed")
	}
}

// Only the standbys' own networks get a replication line (a marked block of
// pg_hba.conf, replaced as a whole); the last one's going removes the block,
// closes the role and drops its password, so the next standby mints a new one.
func TestSyncReplHBA(t *testing.T) {
	w := setup(t, "postgres")
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m1", []string{"10.208.1.0/24"}))
	before, err := w.f.Instances.GetByTile(ctx, w.db.ID)
	must(t, err)
	n := len(w.fake.Calls())
	must(t, w.f.SyncReplHBA(ctx, w.db, []string{"10.208.1.0/24"}))
	if m, _ := w.f.Instances.GetByTile(ctx, w.db.ID); m.ReplPassword != before.ReplPassword {
		t.Error("the password changed while a standby is left")
	}
	must(t, w.f.SyncReplHBA(ctx, w.db, nil))
	var argv, stdin strings.Builder
	for _, c := range w.fake.Calls()[n:] {
		argv.WriteString(strings.Join(c.Args, " ") + "\n")
		stdin.WriteString(c.Stdin)
	}
	contains(t, "stdin", stdin.String(), "ALTER ROLE stackr_repl NOLOGIN PASSWORD NULL")
	contains(t, "argv", argv.String(), "# stackr replication begin", "pg_reload_conf()")
	if m, _ := w.f.Instances.GetByTile(ctx, w.db.ID); m.ReplPassword != "" {
		t.Error("the replication password was kept")
	}
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m2", []string{"10.208.1.0/24"}))
	if m, _ := w.f.Instances.GetByTile(ctx, w.db.ID); m.ReplPassword == "" || m.ReplPassword == before.ReplPassword {
		t.Error("the next standby did not get a new password")
	}
}

// The slot cap and the budget for adding a standby follow the free disk.
func TestWALBudget(t *testing.T) {
	w := setup(t, "postgres")
	free, total := 0, 0
	w.f.NodeDiskMB = func(context.Context, store.Tile) int { return free }
	w.f.NodeDiskTotalMB = func(context.Context, store.Tile) int { return total }
	for _, c := range []struct {
		free, total int
		refused     bool
	}{{0, 0, false}, {100 << 10, 200 << 10, false}, {6 << 10, 100 << 10, true}, {20 << 10, 100 << 10, false}} {
		free, total = c.free, c.total
		if err := w.f.WALBudget(ctx, w.db); (err != nil) != c.refused {
			t.Errorf("free %d total %d: err = %v, want refused %v", c.free, c.total, err, c.refused)
		}
	}
}

// The slot cap in the running primary's owned conf follows the free disk: the
// file is rewritten and reloaded only when it differs from what it should hold.
func TestSyncOwned(t *testing.T) {
	w := setup(t, "postgres")
	free := 100 << 10
	w.f.NodeDiskMB = func(context.Context, store.Tile) int { return free }
	w.f.NodeDiskTotalMB = func(context.Context, store.Tile) int { return 200 << 10 }
	have := ""
	w.fake.ExecFn = func(cmd []string) string {
		if len(cmd) > 3 && strings.Contains(cmd[2], `cat "$0"`) {
			return have
		}
		return ""
	}
	writes := func() int {
		n := 0
		for _, c := range w.fake.Calls() {
			if c.Method == "Exec" && strings.Contains(strings.Join(c.Args, " "), "$1.new") && slices.Contains(c.Args, "stackr-owned.conf") {
				n++
			}
		}
		return n
	}
	must(t, w.f.SyncOwned(ctx, w.db, ""))
	if writes() != 1 {
		t.Fatalf("an empty owned conf was not written: %d", writes())
	}
	have = "# written by stackr; edits here are overwritten\nmax_slot_wal_keep_size = '10240MB'\n"
	must(t, w.f.SyncOwned(ctx, w.db, ""))
	if writes() != 1 {
		t.Errorf("a current owned conf was rewritten: %d", writes())
	}
	free = 300 << 10 // the disk grew: 10% of it is a bigger cap
	must(t, w.f.SyncOwned(ctx, w.db, ""))
	if writes() != 2 {
		t.Errorf("a stale cap was not rewritten: %d", writes())
	}
	have = "# written by stackr; edits here are overwritten\nmax_slot_wal_keep_size = '30720MB'\n"
	must(t, w.f.SyncOwned(ctx, w.db, mflow.SyncStandbyNames([]string{"stackr_a"}))) // names asked for: the conf is stale
	if writes() != 3 {
		t.Errorf("a changed sync setting was not rewritten: %d", writes())
	}
}
