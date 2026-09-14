package managedtiles

import (
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The data browser runs psql --csv inside the db container, same transport
// as provisioning, so it needs no driver and no network path to the instance.
// Scoping is enforced by credentials: the browser connects as whatever user
// the caller resolves (instance superuser, or a provision's own user), and
// postgres grants do the rest.

// PGNull is the sentinel psql prints for NULL so csv parsing can tell it from
// an empty string. a real cell holding exactly this glyph reads as
// NULL; pick a longer marker if that ever bites.
const PGNull = "␀" // ␀

// PGCreds is one resolved connection scope.
type PGCreds struct {
	User, Password, DB string
}

// pgCSV runs one SQL statement via psql --csv and returns the parsed records
// (header first). stderr stays out of the stream, so NOTICEs can't corrupt it.
func (s *Service) pgCSV(ctx context.Context, instance *repo.Tile, cr PGCreds, query string) ([][]string, error) {
	cid, node, err := s.locate(ctx, instance)
	if err != nil || cid == "" {
		return nil, fmt.Errorf("instance %s is not running", instance.Slug)
	}
	rd, wait, err := s.c.ExecStream(ctx, node, cid, []string{
		"env", "PGPASSWORD=" + cr.Password,
		"psql", "--csv", "-v", "ON_ERROR_STOP=1", "-P", "null=" + PGNull,
		"-U", cr.User, "-d", cr.DB, "-c", query,
	}, nil)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(rd)
	r.FieldsPerRecord = -1
	recs, cerr := r.ReadAll()
	if werr := wait(); werr != nil {
		return nil, pgError(werr)
	}
	if cerr != nil {
		return nil, cerr
	}
	return recs, nil
}

// pgError strips the exec wrapper down to the psql message ("ERROR: ...").
func pgError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "ERROR:"); i >= 0 {
		return fmt.Errorf("%s", msg[i:])
	}
	return err
}

// pgQuoteIdent quotes an identifier.
func pgQuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// pgLit quotes a text literal.
func pgLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// pgTable quotes a "schema.table" name (bare names default to public).
func pgTable(t string) string {
	schema, name, ok := strings.Cut(t, ".")
	if !ok {
		schema, name = "public", schema
	}
	return pgQuoteIdent(schema) + "." + pgQuoteIdent(name)
}

// PGTables lists the base tables the connection can see, as "schema.table".
func (s *Service) PGTables(ctx context.Context, instance *repo.Tile, cr PGCreds) ([]string, error) {
	recs, err := s.pgCSV(ctx, instance, cr,
		`SELECT table_schema || '.' || table_name FROM information_schema.tables
		 WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
		 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, r := range recs {
		if i == 0 || len(r) == 0 {
			continue // header
		}
		out = append(out, r[0])
	}
	return out, nil
}

// PGSelect returns one page of a table: column names (without the leading
// ctid), rows (each row[0] is the ctid), and the total row count.
func (s *Service) PGSelect(ctx context.Context, instance *repo.Tile, cr PGCreds, table string, limit, offset int) (cols []string, rows [][]string, total int64, err error) {
	recs, err := s.pgCSV(ctx, instance, cr, fmt.Sprintf(
		"SELECT count(*) FROM %s", pgTable(table)))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(recs) > 1 && len(recs[1]) > 0 {
		total, _ = strconv.ParseInt(recs[1][0], 10, 64)
	}
	recs, err = s.pgCSV(ctx, instance, cr, fmt.Sprintf(
		"SELECT ctid::text, * FROM %s ORDER BY ctid LIMIT %d OFFSET %d",
		pgTable(table), limit, offset))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(recs) == 0 {
		return nil, nil, total, nil
	}
	return recs[0][1:], recs[1:], total, nil
}

// PGUpdateCell sets one column of the row at ctid (null=true writes NULL).
func (s *Service) PGUpdateCell(ctx context.Context, instance *repo.Tile, cr PGCreds, table, ctid, col, value string, null bool) error {
	v := pgLit(value)
	if null {
		v = "NULL"
	}
	_, err := s.pgCSV(ctx, instance, cr, fmt.Sprintf(
		"UPDATE %s SET %s = %s WHERE ctid = %s", pgTable(table), pgQuoteIdent(col), v, pgLit(ctid)))
	return err
}

// PGDeleteRow removes the row at ctid.
func (s *Service) PGDeleteRow(ctx context.Context, instance *repo.Tile, cr PGCreds, table, ctid string) error {
	_, err := s.pgCSV(ctx, instance, cr, fmt.Sprintf(
		"DELETE FROM %s WHERE ctid = %s", pgTable(table), pgLit(ctid)))
	return err
}

// PGInsertRow inserts one row from the filled columns; empty inputs are left
// to their column defaults. No filled columns inserts an all-defaults row.
func (s *Service) PGInsertRow(ctx context.Context, instance *repo.Tile, cr PGCreds, table string, cols, values []string) error {
	if len(cols) == 0 {
		_, err := s.pgCSV(ctx, instance, cr, fmt.Sprintf(
			"INSERT INTO %s DEFAULT VALUES", pgTable(table)))
		return err
	}
	idents := make([]string, len(cols))
	lits := make([]string, len(values))
	for i, c := range cols {
		idents[i] = pgQuoteIdent(c)
	}
	for i, v := range values {
		lits[i] = pgLit(v)
	}
	_, err := s.pgCSV(ctx, instance, cr, fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		pgTable(table), strings.Join(idents, ", "), strings.Join(lits, ", ")))
	return err
}
