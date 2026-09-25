package db_test

import (
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// An org delete removes everything under it through SQL alone: FK cascades
// for real parent ids, triggers for the polymorphic scopes.
func TestOrgDeleteCascades(t *testing.T) {
	db := servicetest.Store(t).DB()
	now := time.Now()
	for _, q := range []struct {
		sql  string
		args []any
	}{
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
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	if _, err := db.Exec(`DELETE FROM orgs WHERE id = 'o1'`); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"org_members",
		"invites",
		"api_keys",
		"stacks",
		"environments",
		"params",
		"volumes",
		"credentials",
		"connectors",
		"backup_destinations",
		"positions",
		"annotations",
	} {
		var n int
		if err := db.Get(&n, "SELECT count(*) FROM "+table); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows left after org delete", table, n)
		}
	}
	var users int
	if err := db.Get(&users, "SELECT count(*) FROM users"); err != nil || users != 1 {
		t.Errorf("users = %d, %v; an org delete must not remove its members' accounts", users, err)
	}
}
