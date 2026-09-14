package main

// dumpschema migrates a throwaway database to head with the project's own
// embedded migrations and writes out what it ends up with: the schema in
// creation order, then a topological drop order for the down migration.
//
// This is how a baseline gets squashed, and the only way that keeps every
// column an ALTER ever added: dump from a migrated database, never retype.
// Kept in the tree rather than thrown away because the next squash wants it
// too.
//
//	go run ./internal/stackrd/store/db/dumpschema /tmp/head.db

import (
	"fmt"
	"os"

	hamrsqlite "github.com/FyrmForge/hamr/pkg/db/sqlite"

	"github.com/FyrmForge/stackr/internal/stackrd/store/db"
)

func main() {
	path := os.Args[1]
	conn, err := hamrsqlite.Connect(path)
	if err != nil {
		panic(err)
	}
	defer func() { _ = conn.Close() }()
	if err := hamrsqlite.Migrate(conn, db.MigrateConfig()); err != nil {
		panic(err)
	}
	var rows []struct {
		Type string  `db:"type"`
		Name string  `db:"name"`
		Tbl  string  `db:"tbl_name"`
		SQL  *string `db:"sql"`
	}
	if err := conn.Select(&rows, `SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' AND tbl_name NOT LIKE 'schema_migrations%' ORDER BY rowid`); err != nil {
		panic(err)
	}
	for _, r := range rows {
		if r.SQL == nil {
			continue // an implicit index from a UNIQUE column
		}
		fmt.Printf("%s;\n", *r.SQL)
	}
	fmt.Println("---SEEDS---")
	var servers []struct {
		ID, Name, Kind, Endpoint, Settings string
	}
	if err := conn.Select(&servers, `SELECT id, name, kind, endpoint, settings FROM servers`); err != nil {
		panic(err)
	}
	for _, s := range servers {
		fmt.Printf("server: %+v\n", s)
	}
	var tables []string
	if err := conn.Select(&tables, `SELECT name FROM sqlite_master WHERE type='table'
		AND name NOT LIKE 'sqlite_%' AND tbl_name NOT LIKE 'schema_migrations%' ORDER BY rowid`); err != nil {
		panic(err)
	}
	// Children before parents, computed from the foreign keys rather than
	// guessed from creation order: 001 created its tables alphabetically, so
	// api_keys exists before the users it references and reverse-creation
	// order drops the parent first.
	refs := map[string][]string{}
	for _, t := range tables {
		var to []string
		if err := conn.Select(&to, `SELECT "table" FROM pragma_foreign_key_list(?)`, t); err != nil {
			panic(err)
		}
		refs[t] = to
	}
	fmt.Println("---DROPORDER---")
	done := map[string]bool{}
	var order []string
	// A table may be dropped once nothing still-present references it, so
	// emit the ones that reference only already-emitted tables, repeatedly.
	// Self-references and any cycle fall out on the last pass.
	for len(order) < len(tables) {
		progress := false
		for _, t := range tables {
			if done[t] {
				continue
			}
			blocked := false
			for _, other := range tables {
				if other == t || done[other] {
					continue
				}
				for _, r := range refs[other] {
					if r == t {
						blocked = true
					}
				}
			}
			if blocked {
				continue
			}
			done[t], progress = true, true
			order = append(order, t)
		}
		if !progress {
			for _, t := range tables {
				if !done[t] {
					done[t] = true
					order = append(order, t)
				}
			}
		}
	}
	for _, t := range order {
		fmt.Printf("DROP TABLE IF EXISTS %s;\n", t)
	}
}
