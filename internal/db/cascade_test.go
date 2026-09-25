package db_test

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

type stmt struct {
	sql  string
	args []any
}

func execAll(t *testing.T, db *sqlx.DB, stmts []stmt) {
	t.Helper()
	for _, q := range stmts {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

func count(t *testing.T, db *sqlx.DB, table string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
		t.Fatal(err)
	}
	return n
}

// tileWithDomains is tile api in shop/dev with two domains: one named by the
// stack's resource, one by the org's.
func tileWithDomains(now time.Time) []stmt {
	return []stmt{
		{`INSERT INTO domain_resources VALUES ('dr0','instance',NULL,NULL,'example.com',0,'',0,?)`, []any{now}},
		{`INSERT INTO domain_resources VALUES ('dr1','org','o1',NULL,'acme.example.com',0,'',1,?)`, []any{now}},
		{`INSERT INTO domain_resources VALUES ('dr2','stack',NULL,'s1','shop.io',0,'',1,?)`, []any{now}},
		{`INSERT INTO tiles VALUES ('t1','s1','e1','api','api','image','','','nginx:1','','','','{}','','','',80,'','','','',0,0,0,0,0,0,'',0,0,'','','','','',1,'manual','','','',0,0,?,?)`, []any{now, now}},
		{`INSERT INTO domains VALUES ('dm1','t1','api.shop.io','/',80,1,0,'',1,'dr2',0,'{}','',?)`, []any{now}},
		{`INSERT INTO domains VALUES ('dm2','t1','api.shop.acme.example.com','/',80,1,0,'',1,'dr1',1,'{}','',?)`, []any{now}},
	}
}

// An org delete removes everything under it through SQL alone: FK cascades
// for real parent ids, triggers for the polymorphic scopes.
func TestOrgDeleteCascades(t *testing.T) {
	db := servicetest.Store(t).DB()
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO users VALUES ('u1','a@b.c','h','n','user',1,'','system',?,?)`, []any{now, now}},
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,?)`, []any{now}},
		{`INSERT INTO org_members VALUES ('m1','o1','u1','owner',?)`, []any{now}},
		{`INSERT INTO invites VALUES ('i1','o1','x@y.z','owner','u1',?,?,NULL)`, []any{now, now}},
		{`INSERT INTO api_keys VALUES ('k1','u1','o1','ci','hash',?)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','','[]',?)`, []any{now}},
		{`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?)`, []any{now}},
		{`INSERT INTO params VALUES ('p1','org','o1','email','key','secret','enc1:x',?,?)`, []any{now, now}},
		{`INSERT INTO params VALUES ('p2','stack','s1','email','key','secret','enc1:x',?,?)`, []any{now, now}},
		{`INSERT INTO params VALUES ('p3','env','e1','email','key','secret','enc1:x',?,?)`, []any{now, now}},
		{`INSERT INTO volumes VALUES ('v1','env','e1',NULL,'pgdata','vol',0,NULL,?)`, []any{now}},
		{`INSERT INTO positions VALUES ('org','o1','stack:s1',0,0), ('stack','s1','env:e1',0,0), ('env','e1','tile:api',0,0)`, nil},
		{`INSERT INTO annotations VALUES ('a1','org','o1','note',0,0,160,60,'hi',?), ('a2','env','e1','box',0,0,90,90,'',?)`, []any{now, now}},
		{`INSERT INTO credentials VALUES ('c1','o1','hub','https://r','u','enc1:x',?)`, []any{now}},
		{`INSERT INTO connectors VALUES ('g1','o1','github','gh','github.com','enc1:x',?)`, []any{now}},
		{`INSERT INTO backup_destinations VALUES ('d1','o1','s3','b','e','r','b','a','enc1:x','enc1:y',0,?)`, []any{now}},
	})
	execAll(t, db, tileWithDomains(now))

	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o1'`); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"org_members",
		"invites",
		"api_keys",
		"stacks",
		"environments",
		"tiles",
		"domains",
		"params",
		"volumes",
		"credentials",
		"connectors",
		"backup_destinations",
		"positions",
		"annotations",
	} {
		if n := count(t, db, table); n != 0 {
			t.Errorf("%s: %d rows left after org delete", table, n)
		}
	}
	if n := count(t, db, "domain_resources"); n != 1 {
		t.Errorf("domain_resources = %d after org delete, want the instance row alone", n)
	}
	if n := count(t, db, "users"); n != 1 {
		t.Errorf("users = %d; an org delete must not remove its members' accounts", n)
	}
}

// A stack delete takes its own resource and its tiles' domains, even one
// named by the org's resource, and leaves the org's resource standing.
func TestStackDeleteCascades(t *testing.T) {
	db := servicetest.Store(t).DB()
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,?)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','','[]',?)`, []any{now}},
		{`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?)`, []any{now}},
	})
	execAll(t, db, tileWithDomains(now))

	if _, err := db.Exec(`DELETE FROM stacks WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"environments", "tiles", "domains"} {
		if n := count(t, db, table); n != 0 {
			t.Errorf("%s: %d rows left after stack delete", table, n)
		}
	}
	var left []string
	if err := db.Select(&left, `SELECT id FROM domain_resources ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0] != "dr0" || left[1] != "dr1" {
		t.Errorf("domain_resources after stack delete = %v, want [dr0 dr1]", left)
	}
}

// A resource a domain names cannot go; the level must match the owner.
func TestDomainResourceGuards(t *testing.T) {
	db := servicetest.Store(t).DB()
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,?)`, []any{now}},
		{`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','','[]',?)`, []any{now}},
		{`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?)`, []any{now}},
	})
	execAll(t, db, tileWithDomains(now))

	if _, err := db.Exec(`DELETE FROM domain_resources WHERE id = 'dr1'`); err == nil {
		t.Error("deleted a resource a domain names")
	}
	if _, err := db.Exec(`DELETE FROM domain_resources WHERE id = 'dr0'`); err != nil {
		t.Errorf("an unnamed resource: %v", err)
	}

	for _, bad := range []string{
		`INSERT INTO domain_resources VALUES ('x1','instance','o1',NULL,'x1.io',0,'',1,?)`,
		`INSERT INTO domain_resources VALUES ('x2','org',NULL,NULL,'x2.io',0,'',1,?)`,
		`INSERT INTO domain_resources VALUES ('x3','org','o1','s1','x3.io',0,'',1,?)`,
		`INSERT INTO domain_resources VALUES ('x4','stack','o1',NULL,'x4.io',0,'',1,?)`,
		`INSERT INTO domain_resources VALUES ('x5','zone',NULL,NULL,'x5.io',0,'',1,?)`,
		`INSERT INTO domain_resources VALUES ('x6','stack',NULL,'s1','shop.io',0,'',1,?)`,
	} {
		if _, err := db.Exec(bad, now); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
