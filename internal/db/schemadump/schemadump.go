// Package schemadump renders a SQLite schema as normalised, deterministic
// text, for a committed snapshot and for diffing two databases.
package schemadump

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var ws = regexp.MustCompile(`\s+`)

// skip reports bookkeeping tables, which are never part of the schema.
func skip(name string) bool {
	return strings.HasPrefix(name, "sqlite_") || name == "schema_migrations" || name == "migrations"
}

func norm(s string) string { return strings.TrimSpace(ws.ReplaceAllString(s, " ")) }

// Dump returns the schema of db: tables (columns, foreign keys, indexes) and
// triggers, sorted by name.
func Dump(db *sql.DB) (string, error) {
	var b strings.Builder
	names, err := col(db, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return "", err
	}
	for _, t := range names {
		if skip(t) {
			continue
		}
		if err := table(db, &b, t); err != nil {
			return "", fmt.Errorf("table %s: %w", t, err)
		}
	}
	rows, err := db.Query(`SELECT name, tbl_name, sql FROM sqlite_master WHERE type='trigger' ORDER BY name`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, tbl, sqlText string
		if err := rows.Scan(&name, &tbl, &sqlText); err != nil {
			return "", err
		}
		if skip(tbl) {
			continue
		}
		fmt.Fprintf(&b, "trigger %s: %s\n", name, norm(sqlText))
	}
	return b.String(), rows.Err()
}

func col(db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type index struct {
	name, origin string
	unique, part int
}

func table(db *sql.DB, b *strings.Builder, t string) error {
	fmt.Fprintf(b, "table %s\n", t)
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`, t)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, typ string
		var nn, pk int
		var dflt sql.NullString
		if err := rows.Scan(&name, &typ, &nn, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		d := "-"
		if dflt.Valid {
			d = norm(dflt.String)
		}
		fmt.Fprintf(b, "  col %s %s notnull=%d default=%s pk=%d\n", name, strings.ToUpper(typ), nn, d, pk)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rows, err = db.Query(`SELECT "table", "from", COALESCE("to",''), on_update, on_delete FROM pragma_foreign_key_list(?)`, t)
	if err != nil {
		return err
	}
	var fks []string
	for rows.Next() {
		var rt, from, to, ou, od string
		if err := rows.Scan(&rt, &from, &to, &ou, &od); err != nil {
			_ = rows.Close()
			return err
		}
		fks = append(fks, fmt.Sprintf("  fk %s -> %s(%s) on_update=%s on_delete=%s", from, rt, to, ou, od))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	sort.Strings(fks)
	for _, f := range fks {
		fmt.Fprintln(b, f)
	}

	rows, err = db.Query(`SELECT name, "unique", origin, partial FROM pragma_index_list(?)`, t)
	if err != nil {
		return err
	}
	var idxs []index
	for rows.Next() {
		var i index
		if err := rows.Scan(&i.name, &i.unique, &i.origin, &i.part); err != nil {
			_ = rows.Close()
			return err
		}
		idxs = append(idxs, i)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var lines []string
	for _, i := range idxs {
		// Expression columns have a NULL name; DESC and COLLATE are in the sort.
		cols, err := col(db, `SELECT COALESCE(name,'<expr>') || CASE WHEN desc THEN ' DESC' ELSE '' END || CASE WHEN coll <> 'BINARY' THEN ' COLLATE ' || coll ELSE '' END FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno`, i.name)
		if err != nil {
			return err
		}
		name := i.name
		if i.origin != "c" {
			name = "(auto)"
		}
		l := fmt.Sprintf("  index %s unique=%d origin=%s partial=%d (%s)", name, i.unique, i.origin, i.part, strings.Join(cols, ","))
		if i.part == 1 {
			var s sql.NullString
			if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, i.name).Scan(&s); err != nil {
				return err
			}
			l += " where " + where(s.String)
		}
		lines = append(lines, l)
	}
	sort.Strings(lines)
	for _, l := range lines {
		fmt.Fprintln(b, l)
	}
	return nil
}

var whereRe = regexp.MustCompile(`(?is)\bWHERE\b(.*)$`)

func where(sqlText string) string {
	if m := whereRe.FindStringSubmatch(sqlText); m != nil {
		return norm(m[1])
	}
	return "?"
}

// Diff returns a line diff of a and b ("- " only in a, "+ " only in b), or ""
// when they are equal.
func Diff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	l := make([][]int, len(x)+1)
	for i := range l {
		l[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else {
				l[i][j] = max(l[i+1][j], l[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i++
			j++
		case j < len(y) && (i == len(x) || l[i][j+1] > l[i+1][j]):
			out.WriteString("+ " + y[j] + "\n")
			j++
		default:
			out.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return out.String()
}
