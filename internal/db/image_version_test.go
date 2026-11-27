package db_test

import (
	"os"
	"testing"
	"time"
)

// 021 adds a version column to release_tiles and images: rows that exist keep
// every value and read ”, and the down migration takes the columns away.
func TestImageVersionMigrationKeepsOldRows(t *testing.T) {
	db := sliceWorld(t)
	read := func(name string) string {
		b, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	execAll(t, db, []stmt{{read("021_image_version.down.sql"), nil}})
	now := time.Now()
	execAll(t, db, []stmt{
		{`INSERT INTO releases (id, stack_id, number, created_at, created_by, message) VALUES ('r1','s1',1,?,'push','old')`, []any{now}},
		{`INSERT INTO release_tiles (id, release_id, slug, repo, branch, commit_sha, digest) VALUES ('t1','r1','api','nginx:1','','','sha256:a')`, nil},
		{`INSERT INTO images (id, ref, digest, last_digest, last_tag, last_error, created_at) VALUES ('i1','nginx:1','sha256:a','sha256:a','','',?)`, []any{now}},
	})
	execAll(t, db, []stmt{{read("021_image_version.up.sql"), nil}})
	var tile, img string
	if err := db.Get(&tile, `SELECT version || digest FROM release_tiles WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&img, `SELECT last_version || last_digest FROM images WHERE id = 'i1'`); err != nil {
		t.Fatal(err)
	}
	if tile != "sha256:a" || img != "sha256:a" {
		t.Errorf("old rows after 021 = %q, %q", tile, img)
	}
}
