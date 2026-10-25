package db_test

import (
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 027 up, down and up again: pins, envs, ingress picks and exit
// rungs move from node ids to placements (default on the controller, one per
// referenced worker or forgotten node), node access is copied, storage tags
// land, and the down maps everything back to node ids.
func TestPlacementsMigrationRoundTrip(t *testing.T) {
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
	step(26 - int(v)) // the shape before 027

	now := time.Now()
	node := func(id, role, tags string, open int, cidr string) stmt {
		return stmt{`INSERT INTO nodes (id,name,slug,role,tags,range_cidr,wg_public_key,wg_address,status,created_at,open_to_all)
			VALUES (?,?,?,?,?,?,'','','online',?,?)`, []any{id, id, id, role, tags, cidr, now, open}}
	}
	env := func(id, stack, slug, nodeID string) stmt {
		return stmt{`INSERT INTO environments VALUES (?,?,?,?,'static',NULL,'{}','',0,?,NULL,'branch','main',1,?,1,?,'')`,
			[]any{id, stack, slug, slug, "net-" + id, now, nodeID}}
	}
	execAll(t, db, []stmt{
		node("ctl", "controller", `["ingress","egress","builder"]`, 1, "controller"),
		node("w1", "worker", `null`, 0, "10.209.0.0/16"),
		node("w2", "worker", `["egress"]`, 1, "10.210.0.0/16"), // referenced by nothing
		node("w3", "worker", `["egress"]`, 1, "10.211.0.0/16"), // a rung only
		{`INSERT INTO forgotten_nodes VALUES ('gone','old-h3',?)`, []any{now}},
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,'w1')`, []any{now}},
		{`INSERT INTO orgs VALUES ('o2','Globex','globex','','','{}',NULL,'',?,'','','','',0,'ctl')`, []any{now}},
		{`INSERT INTO node_orgs VALUES ('w1','o2')`, nil},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','',?,NULL)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s2','o2','Blog','blog','','{}','','','','',?,'w1')`, []any{now}},
		env("e1", "s1", "dev", "w1"),   // on the org's pin
		env("e2", "s1", "prod", "ctl"), // off the pin
		env("e3", "s2", "dev", "w1"),   // on the stack's pin
		env("e4", "s1", "qa", "gone"),  // lost with a forgotten node
		env("e5", "s1", "old", ""),     // from before nodes
		{`INSERT INTO volumes (id, scope_kind, scope_id, instance_id, slug, name, orphaned_at, created_at, node_id) VALUES ('v1','env','e3',NULL,'pgdata','vol',NULL,?,'')`, []any{now}},
		{`INSERT INTO ingress_picks VALUES ('env','e1','w1')`, nil},
		{`INSERT INTO exit_ladders VALUES ('org','o1',0,'w3'), ('org','o1',1,'own'), ('node','w1',0,'ctl')`, nil},
	})
	step(1)

	get := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := db.Get(&s, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	pl := func(name string) string { return get(`SELECT id FROM placements WHERE name = ?`, name) }
	if got := get(`SELECT node_id FROM placements WHERE name = 'default'`); got != "ctl" {
		t.Fatalf("default on %q, want the controller", got)
	}
	if n := count(t, db, "placements"); n != 4 {
		t.Errorf("placements = %d, want default, w1, w3 and old-h3", n)
	}
	if got := get(`SELECT node_id || '/' || open_to_all FROM placements WHERE name = 'w1'`); got != "w1/0" {
		t.Errorf("w1 placement = %s, want on w1 and closed", got)
	}
	if got := get(`SELECT node_id FROM placements WHERE name = 'old-h3'`); got != "gone" {
		t.Errorf("old-h3 placement on %q, want the forgotten node", got)
	}
	if got := get(`SELECT org_id FROM placement_orgs WHERE placement_id = ?`, pl("w1")); got != "o2" {
		t.Errorf("w1 open to %q, want o2", got)
	}
	for _, c := range []struct{ q, want string }{
		{`SELECT placement_id FROM orgs WHERE id = 'o1'`, pl("w1")},
		{`SELECT placement_id FROM orgs WHERE id = 'o2'`, pl("default")},
		{`SELECT placement_id FROM stacks WHERE id = 's2'`, pl("w1")},
		{`SELECT COALESCE(placement_id, '') FROM stacks WHERE id = 's1'`, ""},
		{`SELECT count(*) FROM orgs WHERE node_id IS NOT NULL`, "0"},
		{`SELECT placement_id FROM environments WHERE id = 'e1'`, pl("w1")},
		{`SELECT placement_id FROM environments WHERE id = 'e2'`, pl("default")},
		{`SELECT placement_id FROM environments WHERE id = 'e3'`, pl("w1")},
		{`SELECT placement_id FROM environments WHERE id = 'e4'`, pl("old-h3")},
		{`SELECT placement_id FROM environments WHERE id = 'e5'`, pl("default")},
		{`SELECT node_id FROM environments WHERE id = 'e1'`, "w1"},
		{`SELECT placement_id FROM ingress_picks WHERE scope_id = 'e1'`, pl("w1")},
		{`SELECT group_concat(target_id) FROM (SELECT target_id FROM exit_ladders WHERE scope_id = 'o1' ORDER BY position)`, pl("w3") + ",own"},
		{`SELECT target_id FROM exit_ladders WHERE scope_kind = 'node'`, "ctl"},
		{`SELECT count(*) FROM settings WHERE key = 'server_setup_done_at'`, "1"},
	} {
		if got := get(c.q); got != c.want {
			t.Errorf("%s = %q, want %q", c.q, got, c.want)
		}
	}
	storage := func(id string) bool {
		return get(`SELECT count(*) FROM json_each((SELECT tags FROM nodes WHERE id = ?)) WHERE value = 'storage'`, id) == "1"
	}
	for id, want := range map[string]bool{"ctl": true, "w1": true, "w2": false, "w3": false} {
		if storage(id) != want {
			t.Errorf("%s storage = %v, want %v", id, !want, want)
		}
	}

	step(-1)
	for _, c := range []struct{ q, want string }{
		{`SELECT node_id FROM orgs WHERE id = 'o1'`, "w1"},
		{`SELECT node_id FROM stacks WHERE id = 's2'`, "w1"},
		{`SELECT node_id FROM ingress_picks WHERE scope_id = 'e1'`, "w1"},
		{`SELECT group_concat(node_id) FROM (SELECT node_id FROM exit_ladders WHERE scope_id = 'o1' ORDER BY position)`, "w3,own"},
	} {
		if got := get(c.q); got != c.want {
			t.Errorf("after down %s = %q, want %q", c.q, got, c.want)
		}
	}
	if tables := tableNames(t, db); slices.Contains(tables, "placements") {
		t.Error("down kept placements")
	}
	step(1)
}

func tableNames(t *testing.T, db interface {
	Select(any, string, ...any) error
}) []string {
	t.Helper()
	var out []string
	if err := db.Select(&out, `SELECT name FROM sqlite_master WHERE type = 'table'`); err != nil {
		t.Fatal(err)
	}
	return out
}
