package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 028 drops node-level access (node_orgs, nodes.open_to_all, the
// join token's open_to_all and org_ids, the dead pin columns); its down
// brings the shape back and rebuilds node access from the placements.
func TestDropNodeAccessRoundTrip(t *testing.T) {
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
	step(27 - int(v)) // the shape before 028

	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO nodes (id,name,slug,role,tags,range_cidr,wg_public_key,wg_address,status,created_at,open_to_all)
			VALUES ('w1','w1','w1','worker','[]','10.209.0.0/16','','','online',?,1)`, []any{now}},
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,NULL,NULL)`, []any{now}},
		{`INSERT INTO placements (id,name,node_id,open_to_all,created_at) VALUES ('p1','eu','w1',0,?)`, []any{now}},
		{`INSERT INTO placement_orgs VALUES ('p1','o1')`, nil},
	})
	cols := func(table string) map[string]bool {
		t.Helper()
		var names []string
		if err := db.Select(&names, `SELECT name FROM pragma_table_info('`+table+`')`); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, n := range names {
			out[n] = true
		}
		return out
	}
	hasTable := func(name string) bool {
		var n int
		if err := db.Get(&n, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	step(1)
	if hasTable("node_orgs") {
		t.Error("up kept node_orgs")
	}
	for table, gone := range map[string][]string{
		"nodes": {"open_to_all"}, "join_tokens": {"open_to_all", "org_ids"}, "orgs": {"node_id"}, "stacks": {"node_id"},
	} {
		got := cols(table)
		for _, c := range gone {
			if got[c] {
				t.Errorf("up kept %s.%s", table, c)
			}
		}
	}
	if !cols("orgs")["placement_id"] || count(t, db, "placements") == 0 {
		t.Error("up dropped the placements side")
	}

	step(-1)
	if !hasTable("node_orgs") || !cols("nodes")["open_to_all"] || !cols("orgs")["node_id"] || !cols("join_tokens")["org_ids"] {
		t.Fatal("down did not bring the shape back")
	}
	var open int
	if err := db.Get(&open, `SELECT open_to_all FROM nodes WHERE id = 'w1'`); err != nil || open != 0 {
		t.Errorf("w1 open_to_all after down = %d, %v; want 0 (its placement is closed)", open, err)
	}
	if n := count(t, db, "node_orgs"); n != 1 {
		t.Errorf("node_orgs rows after down = %d, want 1", n)
	}
	step(1)
}
