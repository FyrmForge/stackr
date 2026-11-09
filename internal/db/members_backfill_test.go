package db_test

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The 009 backfill gives only postgres instances a primary member, and
// 'stopped': nothing records whether the user stopped an older instance.
func TestMembersBackfillPostgresOnlyStopped(t *testing.T) {
	db := sliceWorld(t)
	now := time.Now()
	execAll(t, db, []stmt{
		tileRow("obj", "operated", nil, nil, now),
		{`INSERT INTO operated_instances (id, tile_id, engine, admin_user, admin_password, endpoint, created_at) VALUES ('mi2','obj','s3','a','x','',?)`, []any{now}},
		{`DELETE FROM operated_members`, nil},
	})
	b, err := os.ReadFile("migrations/009_operated_members.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, backfill, ok := strings.Cut(string(b), "INSERT INTO operated_members")
	if !ok {
		t.Fatal("no backfill in 009")
	}
	execAll(t, db, []stmt{{"INSERT INTO operated_members" + backfill, nil}})
	var got []struct {
		InstanceID string `db:"instance_id"`
		State      string `db:"state"`
	}
	if err := db.Select(&got, `SELECT instance_id, state FROM operated_members`); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InstanceID != "mi1" || got[0].State != "stopped" {
		t.Errorf("backfilled members = %+v, want mi1 stopped only", got)
	}
}
