package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func fkClean(t *testing.T, db interface {
	Get(any, string, ...any) error
}) {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT count(*) FROM pragma_foreign_key_check"); err != nil || n != 0 {
		t.Fatalf("foreign_key_check = %d rows, %v", n, err)
	}
}

// Migration 005 up, down and up again on a database with live rows that
// reference a connector: the rebuild keeps them, the down drops what the old
// shape cannot hold, and no foreign key is left dangling.
func TestServerConfigMigrationRoundTrip(t *testing.T) {
	db := servicetest.Store(t).DB()
	cfg := appdb.MigrateConfig()
	step := func(n int) {
		t.Helper()
		if err := sqlite.MigrateSteps(db, cfg, n); err != nil {
			t.Fatalf("steps %d: %v", n, err)
		}
		fkClean(t, db)
	}
	// Pin to 004 whatever is newest, so a later migration never changes what
	// this test rolls back.
	v, _, err := sqlite.MigrateVersion(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	step(4 - int(v)) // the shape before 005

	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs (id,name,slug,avatar_path,env_colors,settings,setup_mode,created_at,
			config_connector_id,config_repo,config_branch,config_path)
			VALUES ('o1','Acme','acme','','','{}','',?,'g1','https://github.com/acme/org','','')`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','g1','https://github.com/acme/shop','','',?)`, []any{now}},
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at)
			VALUES ('g1','o1','github','gh','github.com','enc1:x',?)`, []any{now}},
		{`INSERT INTO org_config_plans VALUES ('op1','o1','abc','no changes','{}','pending','',?,NULL)`, []any{now}},
	})

	step(1)
	if n := count(t, db, "connectors"); n != 1 {
		t.Fatalf("connectors = %d after up, want the org's own", n)
	}
	var plan struct {
		Ticked    string `db:"ticked"`
		Confirmed bool   `db:"confirmed"`
	}
	if err := db.Get(&plan, `SELECT ticked, confirmed FROM org_config_plans WHERE id = 'op1'`); err != nil ||
		plan.Ticked != "[]" || plan.Confirmed {
		t.Fatalf("org plan after up = %+v, %v; want ticked [] and not confirmed", plan, err)
	}

	execAll(t, db, []stmt{
		// a server connector: no org, a host the org's connector also uses
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at,share_all)
			VALUES ('g2',NULL,'github','srv','github.com','enc1:x',?,1)`, []any{now}},
		{`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at)
			VALUES ('g3',NULL,'github','srv2','github.com','enc1:x',?)`, []any{now}},
		{`INSERT INTO connector_shares VALUES ('g3','o1')`, nil},
		{`INSERT INTO server_config_plans VALUES ('sp1','abc','no changes','{}','pending','',?,NULL,'local','file','[]',0)`, []any{now}},
		{`UPDATE orgs SET config_connector_id = 'g2' WHERE id = 'o1'`, nil},
		{`UPDATE stacks SET config_connector_id = 'g3' WHERE id = 's1'`, nil},
	})
	// server connector names are unique; an org's connector name is not
	if _, err := db.Exec(`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at)
		VALUES ('g4',NULL,'github','srv','git.example.com','enc1:x',?)`, now); err == nil {
		t.Error("two server connectors share a name")
	}
	if _, err := db.Exec(`INSERT INTO connectors (id,org_id,provider,name,host,config,created_at)
		VALUES ('g5','o1','github','srv','git.example.com','enc1:x',?)`, now); err != nil {
		t.Errorf("an org connector named like a server one: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM connectors WHERE id = 'g5'`); err != nil {
		t.Fatal(err)
	}

	step(-1)
	if n := count(t, db, "connectors"); n != 1 {
		t.Errorf("connectors = %d after down, want the org's own", n)
	}
	var org, stack string
	if err := db.Get(&org, `SELECT config_connector_id FROM orgs WHERE id = 'o1'`); err != nil || org != "" {
		t.Errorf("org binding after down = %q, %v; a dropped server connector's id clears", org, err)
	}
	if err := db.Get(&stack, `SELECT config_connector_id FROM stacks WHERE id = 's1'`); err != nil || stack != "" {
		t.Errorf("stack binding after down = %q, %v", stack, err)
	}
	if n := count(t, db, "org_config_plans"); n != 1 {
		t.Errorf("org_config_plans = %d after down, want 1", n)
	}

	step(1)
	if n := count(t, db, "connectors"); n != 1 {
		t.Errorf("connectors = %d after the second up, want 1", n)
	}
	if n := count(t, db, "server_config_plans"); n != 0 {
		t.Errorf("server_config_plans = %d after down and up, want 0", n)
	}
}

// A server connector's shares go with the connector and with the org.
func TestConnectorSharesCascade(t *testing.T) {
	db := servicetest.Store(t).DB()
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0)`, []any{now}},
		{`INSERT INTO orgs VALUES ('o2','Globex','globex','','','{}',NULL,'',?,'','','','',0)`, []any{now}},
		{`INSERT INTO connectors VALUES ('g1',NULL,'github','srv','github.com','enc1:x',?,0)`, []any{now}},
		{`INSERT INTO connectors VALUES ('g2',NULL,'github','srv2','github.com','enc1:x',?,0)`, []any{now}},
		{`INSERT INTO connector_shares VALUES ('g1','o1'), ('g1','o2'), ('g2','o1')`, nil},
	})
	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o2'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "connector_shares"); n != 2 {
		t.Errorf("connector_shares = %d after an org delete, want 2", n)
	}
	if _, err := db.Exec(`DELETE FROM connectors WHERE id = 'g1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, "connector_shares"); n != 1 {
		t.Errorf("connector_shares = %d after a connector delete, want 1", n)
	}
	if n := count(t, db, "connectors"); n != 1 {
		t.Errorf("connectors = %d; an org delete must not remove a server connector", n)
	}
}
