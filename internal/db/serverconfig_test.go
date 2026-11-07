package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A server connector's shares go with the connector and with the org.
func TestConnectorSharesCascade(t *testing.T) {
	t.Parallel()
	db := servicetest.Store(t).DB()
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,NULL)`, []any{now}},
		{`INSERT INTO orgs VALUES ('o2','Globex','globex','','','{}',NULL,'',?,'','','','',0,NULL)`, []any{now}},
		{`INSERT INTO connectors VALUES ('g1',NULL,'github','srv','github.com','enc1:x',?,0,'["git"]',NULL,'')`, []any{now}},
		{`INSERT INTO connectors VALUES ('g2',NULL,'github','srv2','github.com','enc1:x',?,0,'["git"]',NULL,'')`, []any{now}},
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
