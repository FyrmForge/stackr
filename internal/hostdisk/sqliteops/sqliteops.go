// Package sqliteops is the two SQLite calls the panel database move needs in
// the privileged disk helper: a TRUNCATE checkpoint and a read-only integrity
// check. It is separate from hostdisk, which stays standard library only.
package sqliteops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers "sqlite"
)

func open(path, query string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: path, RawQuery: query}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Checkpoint folds the -wal into the main file and truncates it, so the main
// file alone holds every row. A busy checkpoint (a writer is still there) is
// an error: the copy would miss rows.
func Checkpoint(ctx context.Context, path string) error {
	db, err := open(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	var busy, logFrames, done int
	err = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &done)
	if err == nil && busy != 0 {
		err = errors.New("the database is in use: the checkpoint did not finish")
	}
	return errors.Join(err, db.Close())
}

// Integrity runs integrity_check on a read-only, immutable connection: it
// opens nothing for writing and ignores any -wal beside it.
func Integrity(ctx context.Context, path string) error {
	db, err := open(path, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return errors.Join(err, db.Close())
	}
	var lines []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			break
		}
		lines = append(lines, s)
	}
	err = errors.Join(err, rows.Err(), rows.Close(), db.Close())
	if err == nil && (len(lines) != 1 || lines[0] != "ok") {
		err = fmt.Errorf("integrity_check: %v", lines)
	}
	return err
}
