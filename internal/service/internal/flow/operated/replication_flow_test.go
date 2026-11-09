package operated_test

import (
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
// the member label, restart "no"; the password is in the env, nowhere else.
func TestStandbySpec(t *testing.T) {
	w := setup(t, "postgres")
	slot := mflow.SlotName("m1")
	must(t, w.f.PrepareStandby(ctx, w.db, slot))
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
	for _, want := range []string{"STACKR_PRIMARY=" + host, "STACKR_SLOT=" + slot, "STACKR_REPL_PASS=" + m.ReplPassword} {
		if !slices.Contains(spec.Env, want) {
			t.Errorf("no %s in the env", strings.SplitN(want, "=", 2)[0])
		}
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
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m1"))
	m, err := w.f.Instances.GetByTile(ctx, w.db.ID)
	must(t, err)
	var stdin, argv strings.Builder
	for _, c := range w.fake.Calls() {
		stdin.WriteString(c.Stdin)
		argv.WriteString(strings.Join(c.Args, " ") + "\n")
	}
	contains(t, "stdin", stdin.String(), "CREATE ROLE stackr_repl REPLICATION LOGIN PASSWORD '"+m.ReplPassword+"'", "pg_create_physical_replication_slot('stackr_m1')")
	contains(t, "argv", argv.String(), "stackr-owned.conf", "host replication stackr_repl 10.208.0.0/12 scram-sha-256", "pg_reload_conf()")
	if strings.Contains(argv.String(), m.ReplPassword) {
		t.Error("the replication password is on an argv")
	}
	// a second standby re-syncs: the password stays
	must(t, w.f.PrepareStandby(ctx, w.db, "stackr_m2"))
	if m2, _ := w.f.Instances.GetByTile(ctx, w.db.ID); m2.ReplPassword != m.ReplPassword {
		t.Error("the replication password changed")
	}
}
