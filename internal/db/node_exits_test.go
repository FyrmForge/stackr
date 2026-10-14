package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 024 up, down and up again: every node gains the egress tag (once),
// exit_ladders admits a 'node' scope that dies with its node, and the down
// drops those rungs and keeps the rest.
func TestNodeExitsMigrationRoundTrip(t *testing.T) {
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
	step(23 - int(v)) // the shape before 024

	now := time.Now()
	node := func(id, role, tags, cidr string) stmt {
		return stmt{`INSERT INTO nodes (id,name,slug,role,tags,range_cidr,wg_public_key,wg_address,status,created_at)
			VALUES (?,?,?,?,?,?,'','','online',?)`, []any{id, id, id, role, tags, cidr, now}}
	}
	execAll(t, db, []stmt{
		node("a", "controller", `["ingress"]`, "10.213.0.0/16"),
		node("b", "worker", `["egress","builder"]`, "10.214.0.0/16"),
		node("c", "worker", `null`, "10.215.0.0/16"),
		{`INSERT INTO exit_ladders VALUES ('org','o1',0,'b')`, nil},
	})
	step(1)

	tags := func(id string) string {
		var s string
		if err := db.Get(&s, `SELECT tags FROM nodes WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := tags("a"); got != `["ingress","egress"]` {
		t.Fatalf("a tags = %s, want egress added", got)
	}
	if got := tags("b"); got != `["egress","builder"]` {
		t.Fatalf("b tags = %s, want untouched", got)
	}
	if got := tags("c"); got != `["egress"]` {
		t.Fatalf("c tags = %s, want egress on a node whose tags are JSON null", got)
	}
	execAll(t, db, []stmt{{`INSERT INTO exit_ladders VALUES ('node','a',0,'b')`, nil}})
	if n := count(t, db, "exit_ladders"); n != 2 {
		t.Fatalf("rungs = %d after up, want the org rung kept and a node rung added", n)
	}
	execAll(t, db, []stmt{{`DELETE FROM nodes WHERE id = 'a'`, nil}})
	if n := count(t, db, "exit_ladders"); n != 1 {
		t.Fatalf("rungs = %d after deleting the node, want its own gone", n)
	}

	execAll(t, db, []stmt{{`INSERT INTO exit_ladders VALUES ('node','b',0,'b')`, nil}})
	step(-1) // down drops node rungs
	if n := count(t, db, "exit_ladders"); n != 1 {
		t.Fatalf("rungs = %d after down, want only the org rung", n)
	}
	if _, err := db.Exec(`INSERT INTO exit_ladders VALUES ('node','b',0,'b')`); err == nil {
		t.Fatal("down still admits a node scope")
	}
	step(1)
}
