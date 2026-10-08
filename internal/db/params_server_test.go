package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// Migration 006 up, down and up again on a database with live params: the
// rebuild keeps every org row, the server scope is accepted after it, the
// down drops the server rows only, and the scope triggers still cascade.
func TestServerParamsMigrationRoundTrip(t *testing.T) {
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
	step(5 - int(v)) // the shape before 006

	now := time.Now()
	param := func(id, kind, scope, col string) stmt {
		return stmt{`INSERT INTO params VALUES (?,?,?,?,'n','param','v',?,?)`, []any{id, kind, scope, col, now, now}}
	}
	execAll(t, db, []stmt{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o1','Acme','acme','','','{}','',?,'','','','')`, []any{now}},
		param("p1", "org", "o1", "a"),
	})
	if _, err := db.Exec(`INSERT INTO params VALUES ('x','server','server','a','n','param','v',?,?)`, now, now); err == nil {
		t.Fatal("005 accepted a server param")
	}

	step(1)
	execAll(t, db, []stmt{param("p2", "server", "server", "s3")})
	if _, err := db.Exec(`INSERT INTO params VALUES ('x','bogus','x','a','n','param','v',?,?)`, now, now); err == nil {
		t.Error("an unknown scope kind is accepted")
	}
	if _, err := db.Exec(`INSERT INTO params VALUES ('y','server','server','s3','n','param','v',?,?)`, now, now); err == nil {
		t.Error("a duplicate server param is accepted")
	}
	if n := count(t, db, "params"); n != 2 {
		t.Fatalf("params = %d after up, want the org row and the server row", n)
	}
	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "params"); n != 1 {
		t.Errorf("params = %d after an org delete, want only the server row", n)
	}

	execAll(t, db, []stmt{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o2','Globex','globex','','','{}','',?,'','','','')`, []any{now}},
		param("p3", "org", "o2", "b"),
	})
	step(-1)
	if n := count(t, db, "params"); n != 1 {
		t.Errorf("params = %d after down, want the org row only", n)
	}
	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o2'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "params"); n != 0 {
		t.Errorf("params = %d; the org cascade trigger is gone after down", n)
	}
	step(1)
	if n := count(t, db, "params"); n != 0 {
		t.Errorf("params = %d after the second up, want 0", n)
	}
}
