package db_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/db"
)

// 001_initial is frozen (2026-09-18). Every migration after it is forward-only
// and additive: a rename or a drop against a database holding someone's data is
// the one mistake with no undo, and by the time it runs the baseline is long
// gone, so there is nothing to wipe and start again from. The deny list is a
// test rather than a convention because a convention is what nobody reads at
// the moment they are adding a column.
//
// A drop we actually want is two releases: stop writing the column, then remove
// it once no running version reads it. The second release carries the escape
// hatch below.
var destructive = []*regexp.Regexp{
	// Bare verbs rather than keyword pairs. SQLite makes COLUMN optional, so
	// `ALTER TABLE t DROP b` and `ALTER TABLE t RENAME a TO c` both really
	// destroy while `drop column` and `rename to` match neither, and DROP VIEW
	// and DROP TRIGGER were uncovered for the same reason. Matching the verb
	// alone catches every spelling and costs only a false positive on a column
	// literally named "drop", which the escape hatch answers.
	regexp.MustCompile(`(?i)\bdrop\b`),
	regexp.MustCompile(`(?i)\brename\b`),
	regexp.MustCompile(`(?i)\btruncate\b`),
	regexp.MustCompile(`(?i)\bdelete\s+from\b`),
}

// allowLine is the escape hatch: put it immediately above the statement and say
// why. Deliberate destruction stays possible and stays visible in review.
// Immediately means immediately: a blank line or another comment between the
// two clears it, so a header block quoting this syntax cannot exempt the
// file's first statement.
var allowLine = regexp.MustCompile(`(?i)^\s*--\s*migration-guard:\s*allow\s+\S`)

// scanDestructive returns what an up migration does that it must not. It strips
// comments and single-quoted strings first, so the word "drop" in a note or in
// a default value does not trip it, and an allow comment suppresses everything
// up to the end of that statement.
//
// ponytail: line-oriented, not a parser. A statement wrapped so that `DROP` and
// `TABLE` land on separate lines is not caught (harmless now that the verbs are
// matched alone), and a string literal spanning lines can trip a false positive
// because quote state does not carry across them. Both are cheap to hit in
// review and expensive to fix properly; write a real statement splitter only if
// a migration ever actually formats that way.
func scanDestructive(sql string) []string {
	var found []string
	allowed := false
	for _, line := range strings.Split(sql, "\n") {
		if allowLine.MatchString(line) {
			allowed = true
			continue
		}
		code := stripSQL(line)
		if strings.TrimSpace(code) == "" {
			// Blank, or a comment. Either way the allow line is no longer
			// immediately above anything.
			allowed = false
			continue
		}
		if !allowed {
			for _, re := range destructive {
				if m := re.FindString(code); m != "" {
					found = append(found, strings.Join(strings.Fields(m), " "))
				}
			}
		}
		// The hatch covers one statement, not the rest of the file.
		if strings.Contains(code, ";") {
			allowed = false
		}
	}
	return found
}

// stripSQL drops -- comments and the contents of single-quoted strings.
func stripSQL(line string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(line); i++ {
		switch {
		case inStr:
			if line[i] == '\'' {
				inStr = false
			}
		case line[i] == '\'':
			inStr = true
		case line[i] == '-' && i+1 < len(line) && line[i+1] == '-':
			return b.String()
		default:
			b.WriteByte(line[i])
		}
	}
	return b.String()
}

func TestMigrationsAreAdditiveAfterTheBaseline(t *testing.T) {
	fsys := db.MigrateConfig().FS
	names, err := fs.Glob(fsys, "migrations/*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, names, "no migrations found, the glob or the directory moved")

	for _, name := range names {
		// The baseline predates the freeze: it creates the world, and its own
		// down migration drops it. Only what lands on top of it is bound.
		if strings.Contains(name, "001_initial") {
			continue
		}
		b, err := fs.ReadFile(fsys, name)
		require.NoError(t, err, name)
		assert.Empty(t, scanDestructive(string(b)),
			"%s: 001_initial is frozen and migrations after it must be additive. "+
				"A deliberate drop needs a `-- migration-guard: allow <reason>` line above the statement.", name)
	}
}

// The suite above is vacuous while 001_initial is the only migration, so the
// scanner is proved against input of its own.
func TestDestructiveScanner(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"add column is fine", "ALTER TABLE tiles ADD COLUMN note TEXT NOT NULL DEFAULT '';", false},
		{"drop table", "DROP TABLE tiles;", true},
		{"drop column", "ALTER TABLE tiles DROP COLUMN note;", true},
		{"drop index", "DROP INDEX idx_tiles_slug;", true},
		{"rename table", "ALTER TABLE tiles RENAME TO boxes;", true},
		{"rename column", "ALTER TABLE tiles RENAME COLUMN note TO memo;", true},
		{"delete from", "DELETE FROM tiles WHERE slug = 'x';", true},
		{"truncate", "TRUNCATE tiles;", true},
		{"bare drop, no COLUMN keyword", "ALTER TABLE tiles DROP note;", true},
		{"bare rename, no COLUMN keyword", "ALTER TABLE tiles RENAME note TO memo;", true},
		{"drop view", "DROP VIEW v_tiles;", true},
		{"drop trigger", "DROP TRIGGER trg_tiles;", true},
		{"the word in a comment", "-- this does not drop table tiles\nALTER TABLE tiles ADD COLUMN a TEXT;", false},
		{"the word in a string", "INSERT INTO notes (body) VALUES ('drop table tiles');", false},
		{"allowed with a reason", "-- migration-guard: allow column unused since v2.1\nALTER TABLE tiles DROP COLUMN note;", false},
		{"allow does not cover the next statement", "-- migration-guard: allow one drop\nDROP TABLE a;\nDROP TABLE b;", true},
		{"allow needs a reason", "-- migration-guard: allow\nDROP TABLE a;", true},
		{"allow does not reach across a blank line", "-- migration-guard: allow gone in v2.1\n\nDROP TABLE a;", true},
		{"allow does not reach across another comment", "-- migration-guard: allow gone in v2.1\n-- and here is why\nDROP TABLE a;", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanDestructive(tc.sql)
			assert.Equal(t, tc.want, len(got) > 0, "scanned %q -> %v", tc.sql, got)
		})
	}
}

// baselineHashes pins 001_initial, frozen 2026-09-18. The additive-only guard
// above skips the baseline, and golang-migrate records only a version integer
// with no checksum, so nothing else notices an edit here: a database already at
// version 1 never re-runs the file, and a query written against the edited
// baseline fails at runtime against every database created from the old one.
//
// Changing the baseline is therefore changing these constants too, which is the
// point. If you are here because a comment typo broke the build, the answer is
// still a 002: this file is closed.
var baselineHashes = map[string]string{
	"migrations/001_initial.up.sql":   "b1e2b5a5aa6792198b264bb5e56565a178012ce997e7cbfe61cef2e88940fefd",
	"migrations/001_initial.down.sql": "4f15e97dacef5b239aa8af2f438ec754895558a2cf9a4d2c77beb18f04268b06",
}

func TestBaselineIsFrozen(t *testing.T) {
	fsys := db.MigrateConfig().FS
	for name, want := range baselineHashes {
		b, err := fs.ReadFile(fsys, name)
		require.NoError(t, err, name)
		sum := sha256.Sum256(b)
		assert.Equal(t, want, hex.EncodeToString(sum[:]),
			"%s changed. It was frozen on 2026-09-18: a schema change is a new 002 on top of it, "+
				"never an edit here, because every database already created from this file keeps the old shape.", name)
	}
}
