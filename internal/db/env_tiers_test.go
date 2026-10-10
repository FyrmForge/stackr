package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 010 up, down and up again: tiers and the env lock appear, the
// params CHECK takes the tier scopes, a tier delete takes its params, and
// the down drops the new scopes' rows and keeps the rest.
func TestEnvTiersMigrationRoundTrip(t *testing.T) {
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
	step(9 - int(v)) // the shape before 010

	now := time.Now()
	param := func(id, kind, scope string) stmt {
		return stmt{`INSERT INTO params VALUES (?,?,?,'a','n','param','v',?,?)`, []any{id, kind, scope, now, now}}
	}
	execAll(t, db, []stmt{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o1','Acme','acme','','','{}','',?,'','','','')`, []any{now}},
		param("p1", "org", "o1"),
		param("p2", "env", "e1"),
		param("p0", "stack", "s1"),
	})
	if _, err := db.Exec(`INSERT INTO params VALUES ('x','tier','t1','a','n','param','v',?,?)`, now, now); err == nil {
		t.Fatal("009 accepted a tier param")
	}

	step(1)
	if n := count(t, db, "params"); n != 2 {
		t.Errorf("params = %d after up, want the stack row deleted", n)
	}
	if _, err := db.Exec(`INSERT INTO params VALUES ('x','stack','s1','a','n','param','v',?,?)`, now, now); err == nil {
		t.Error("010 accepted a stack param")
	}
	execAll(t, db, []stmt{
		{`INSERT INTO tiers VALUES ('t1','o1','prod',0,1,?)`, []any{now}},
		param("p3", "tier", "t1"),
		param("p4", "stack_pr", "s1"),
		param("p5", "org_pr", "o1"),
	})
	if _, err := db.Exec(`INSERT INTO tiers VALUES ('t2','o1','prod',1,1,?)`, now); err == nil {
		t.Error("a duplicate tier slug is accepted")
	}
	if _, err := db.Exec(`INSERT INTO params VALUES ('x','bogus','x','a','n','param','v',?,?)`, now, now); err == nil {
		t.Error("an unknown scope kind is accepted")
	}
	if _, err := db.Exec(`DELETE FROM tiers WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "params"); n != 4 {
		t.Errorf("params = %d after a tier delete, want its row gone", n)
	}

	step(-1)
	if n := count(t, db, "params"); n != 2 {
		t.Errorf("params = %d after down, want the org and env rows", n)
	}
	var cols int
	if err := db.Get(&cols, `SELECT count(*) FROM pragma_table_info('environments') WHERE name = 'locked'`); err != nil || cols != 0 {
		t.Errorf("environments.locked after down: %d, %v", cols, err)
	}
	step(1)
}
