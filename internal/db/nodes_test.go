package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 011 down to 010 and up again: the nodes table and the placement
// columns come and go, rows made before it keep their data, and one
// controller row is all the unique index admits.
func TestNodesMigrationRoundTrip(t *testing.T) {
	db := servicetest.Store(t).DB()
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
	step(10 - int(v)) // the shape before 011

	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','',?)`, []any{now}},
		{`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?,1)`, []any{now}},
	})

	step(int(v) - 10)
	var env struct {
		NodeID string `db:"node_id"`
		Subnet string `db:"subnet"`
	}
	if err := db.Get(&env, `SELECT node_id, subnet FROM environments WHERE id = 'e1'`); err != nil || env != struct {
		NodeID string `db:"node_id"`
		Subnet string `db:"subnet"`
	}{} {
		t.Fatalf("env after up: %+v, %v", env, err)
	}
	node := func(id, role string) error {
		_, err := db.Exec(`INSERT INTO nodes VALUES (?,?,?,?,'[]',?,'','','online',?)`,
			id, id, id, role, "10.213.0.0/16", now)
		return err
	}
	if err := node("a", "controller"); err != nil {
		t.Fatal(err)
	}
	if err := node("b", "controller"); err == nil {
		t.Fatal("second controller accepted")
	}
	execAll(t, db, []stmt{
		{`UPDATE orgs SET node_id = 'a' WHERE id = 'o1'`, nil},
		{`DELETE FROM nodes WHERE id = 'a'`, nil},
	})
	var pin *string
	if err := db.Get(&pin, `SELECT node_id FROM orgs WHERE id = 'o1'`); err != nil || pin != nil {
		t.Fatalf("pin after node delete = %v, %v; want NULL", pin, err)
	}

	step(10 - int(v)) // down again with a node-shaped schema
	step(int(v) - 10) // and up
}
