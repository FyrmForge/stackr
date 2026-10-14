package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 025 up, down and up again: a deleted env keeps its volume row,
// orphaned and pinned to the env's node, without its schedule; backup groups
// null their runs' group_id; jobs admits 'skipped'.
func TestNodeLifecycleMigrationRoundTrip(t *testing.T) {
	st := servicetest.Store(t)
	db := st.DB()
	cfg := appdb.MigrateConfig()
	step := func(n int) {
		t.Helper()
		if err := sqlite.MigrateSteps(db, cfg, n); err != nil {
			t.Fatalf("steps %d: %v", n, err)
		}
		fkClean(t, db)
	}
	v, _, err := sqlite.MigrateVersion(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	step(24 - int(v)) // the shape before 025

	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,NULL)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','',?,NULL)`, []any{now}},
		{`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?,1,'','')`, []any{now}},
		{`UPDATE environments SET node_id = 'n1' WHERE id = 'e1'`, nil},
		{`INSERT INTO volumes VALUES ('v1','env','e1',NULL,'pgdata','vol',0,NULL,?)`, []any{now}},
		{`INSERT INTO volumes VALUES ('v2','stack','s1',NULL,'shared','vol',0,NULL,?)`, []any{now}},
		{`INSERT INTO backup_schedules VALUES ('bs1','v1','tar',NULL,'0 0 * * *','UTC',3,'full',?)`, []any{now}},
	})
	step(1)

	ctx := context.Background()
	execAll(t, db, []stmt{{`DELETE FROM environments WHERE id = 'e1'`, nil}})
	v1, err := st.Volumes.Get(ctx, "v1")
	if err != nil {
		t.Fatalf("env volume gone after env delete: %v", err)
	}
	if v1.OrphanedAt == nil || v1.NodeID != "n1" {
		t.Fatalf("v1 = orphaned %v node %q, want orphaned on n1", v1.OrphanedAt, v1.NodeID)
	}
	if n := count(t, db, "backup_schedules"); n != 0 {
		t.Fatalf("schedules = %d, want the orphan's gone", n)
	}
	if got, err := st.Volumes.ListByNode(ctx, "n1"); err != nil || len(got) != 1 || got[0].ID != "v1" {
		t.Fatalf("ListByNode(n1) = %v, %v", got, err)
	}

	execAll(t, db, []stmt{{`DELETE FROM stacks WHERE id = 's1'`, nil}})
	v2, err := st.Volumes.Get(ctx, "v2")
	if err != nil || v2.OrphanedAt == nil || v2.NodeID != "" {
		t.Fatalf("v2 = %+v, %v; want orphaned on the controller", v2, err)
	}

	execAll(t, db, []stmt{
		{`INSERT INTO jobs VALUES ('j1','x','skipped',NULL,'[]','{}',NULL,'','',?,NULL,NULL)`, []any{now}},
		{`INSERT INTO backup_destinations VALUES ('d1',NULL,'local','b','','','','','','',0,?)`, []any{now}},
		{`INSERT INTO backup_groups VALUES ('g1','n1','h1','node h1','manual','d1','','ok','',?,NULL)`, []any{now}},
		{`INSERT INTO backup_runs (id,kind,volume_id,schedule_id,dest_id,trigger,status,object_key,size_bytes,error,created_at,group_id)
			VALUES ('r1','volume',NULL,NULL,'d1','manual','ok','k',0,'',?,'g1')`, []any{now}},
	})
	fkClean(t, db)
	execAll(t, db, []stmt{{`DELETE FROM backup_groups WHERE id = 'g1'`, nil}})
	var gid *string
	if err := db.Get(&gid, `SELECT group_id FROM backup_runs WHERE id = 'r1'`); err != nil || gid != nil {
		t.Fatalf("group_id = %v, %v; want NULL after the group is deleted", gid, err)
	}

	step(-1)
	var state string
	if err := db.Get(&state, `SELECT state FROM jobs WHERE id = 'j1'`); err != nil || state != "cancelled" {
		t.Fatalf("skipped job after down = %q, %v; want cancelled", state, err)
	}
	if _, err := db.Exec(`UPDATE jobs SET state = 'skipped'`); err == nil {
		t.Fatal("down still admits skipped")
	}
	execAll(t, db, []stmt{{`DELETE FROM volumes WHERE id = 'v2'`, nil}})
	step(1)
}
