package db_test

import (
	"os"
	"testing"
	"time"
)

// 019 adds two nullable columns: releases that exist keep every value and read
// NULL, and the down migration takes the columns away again.
func TestReleaseLinesMigrationKeepsOldReleases(t *testing.T) {
	db := sliceWorld(t)
	read := func(name string) string {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	execAll(t, db, []stmt{{read("019_release_lines.down.sql"), nil}})
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO releases (id, stack_id, number, created_at, created_by, message) VALUES ('r1','s1',1,?,'push','old')`, []any{now}},
	})
	execAll(t, db, []stmt{{read("019_release_lines.up.sql"), nil}})
	var got struct {
		Message string  `db:"message"`
		BuiltOn *string `db:"built_on"`
		Line    *string `db:"line"`
	}
	if err := db.Get(&got, `SELECT message, built_on, line FROM releases WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if got.Message != "old" || got.BuiltOn != nil || got.Line != nil {
		t.Errorf("old release after 019 = %+v", got)
	}
	execAll(t, db, []stmt{
		{`INSERT INTO releases (id, stack_id, number, created_at, created_by, built_on, line) VALUES ('r2','s1',2,?,'push','r1','branch:main')`, []any{now}},
	})
}
