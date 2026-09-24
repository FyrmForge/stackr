// Package store is CRUD plus type mapping, one file and one small interface
// per table. No defaults, no ORDER BY policy, no status logic: those belong
// to the leaf that owns the table. Encrypted columns are sealed on write and
// opened on read here, so no caller ever sees ciphertext.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
)

// querier is what *sqlx.DB and *sqlx.Tx share, so every table works inside
// and outside a transaction.
type querier interface {
	sqlx.ExtContext
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
}

// Tables is every table, bound to one querier. Store and Tx both embed it.
type Tables struct {
	Users            UserStore
	Sessions         SessionStore
	APIKeys          APIKeyStore
	Orgs             OrgStore
	OrgMembers       OrgMemberStore
	Invites          InviteStore
	Stacks           StackStore
	Environments     EnvironmentStore
	Tiles            TileStore
	Images           ImageStore
	Params           ParamStore
	Settings         SettingStore
	Volumes          VolumeStore
	Domains          DomainStore
	Credentials      CredentialStore
	Connectors       ConnectorStore
	ManagedInstances ManagedInstanceStore
	Provisions       ProvisionStore
	Releases         ReleaseStore
	ReleaseTiles     ReleaseTileStore
	Jobs             JobStore
	BackupDests      BackupDestStore
	BackupSchedules  BackupScheduleStore
	BackupRuns       BackupRunStore
	Runs             RunStore
}

func bind(q querier, box *secrets.Box) Tables {
	return Tables{
		Users:            users{crud[User]{q, box, usersT}},
		Sessions:         sessions{q},
		APIKeys:          apiKeys{crud[APIKey]{q, box, apiKeysT}},
		Orgs:             orgs{crud[Org]{q, box, orgsT}},
		OrgMembers:       orgMembers{crud[OrgMember]{q, box, orgMembersT}},
		Invites:          invites{crud[Invite]{q, box, invitesT}},
		Stacks:           stacks{crud[Stack]{q, box, stacksT}},
		Environments:     environments{crud[Environment]{q, box, environmentsT}},
		Tiles:            tiles{crud[Tile]{q, box, tilesT}},
		Images:           images{crud[Image]{q, box, imagesT}},
		Params:           params{crud[Param]{q, box, paramsT}},
		Settings:         settings{q},
		Volumes:          volumes{crud[Volume]{q, box, volumesT}},
		Domains:          domains{crud[Domain]{q, box, domainsT}},
		Credentials:      credentials{crud[Credential]{q, box, credentialsT}},
		Connectors:       connectors{crud[Connector]{q, box, connectorsT}},
		ManagedInstances: managedInstances{crud[ManagedInstance]{q, box, managedInstancesT}},
		Provisions:       provisions{crud[Provision]{q, box, provisionsT}},
		Releases:         releases{crud[Release]{q, box, releasesT}},
		ReleaseTiles:     releaseTiles{crud[ReleaseTile]{q, box, releaseTilesT}},
		Jobs:             jobs{crud[Job]{q, box, jobsT}},
		BackupDests:      backupDests{crud[BackupDest]{q, box, backupDestsT}},
		BackupSchedules:  backupSchedules{crud[BackupSchedule]{q, box, backupSchedulesT}},
		BackupRuns:       backupRuns{crud[BackupRun]{q, box, backupRunsT}},
		Runs:             runs{crud[Run]{q, box, runsT}},
	}
}

// Store is the database outside a transaction.
type Store struct {
	Tables
	db  *sqlx.DB
	box *secrets.Box
}

// New binds every table to db. box seals the encrypted columns.
func New(db *sqlx.DB, box *secrets.Box) *Store {
	return &Store{Tables: bind(db, box), db: db, box: box}
}

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// DB is the handle for a flow's own query file (joins, with a one-line reason).
func (s *Store) DB() *sqlx.DB { return s.db }

// Tx is every table inside one transaction.
type Tx struct{ Tables }

// Tx runs fn in one transaction: commit on nil, roll back on error or panic.
func (s *Store) Tx(ctx context.Context, fn func(Tx) error) (err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(Tx{bind(tx, s.box)}); err != nil {
		return err
	}
	return tx.Commit()
}

// table is one table's SQL, derived once from T's db tags.
type table[T any] struct {
	name, cols, insert, update string
	// secret returns pointers to T's encrypted fields.
	secret func(*T) []*string
}

func newTable[T any](name string, secret func(*T) []*string) table[T] {
	var cols, named, sets []string
	rt := reflect.TypeFor[T]()
	for i := range rt.NumField() {
		c := rt.Field(i).Tag.Get("db")
		if c == "" || c == "-" {
			continue
		}
		cols = append(cols, c)
		named = append(named, ":"+c)
		if c != "id" {
			sets = append(sets, c+" = :"+c)
		}
	}
	return table[T]{
		name:   name,
		cols:   strings.Join(cols, ", "),
		insert: fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", name, strings.Join(cols, ", "), strings.Join(named, ", ")),
		update: fmt.Sprintf("UPDATE %s SET %s WHERE id = :id", name, strings.Join(sets, ", ")),
		secret: secret,
	}
}

// crud is the shared create/get/update/delete; each table adds its own
// lookups through one and many.
type crud[T any] struct {
	q   querier
	box *secrets.Box
	t   table[T]
}

func (c crud[T]) seal(row *T) error {
	if c.t.secret == nil {
		return nil
	}
	for _, p := range c.t.secret(row) {
		v, err := c.box.Encrypt(*p)
		if err != nil {
			return err
		}
		*p = v
	}
	return nil
}

func (c crud[T]) open(row *T) error {
	if c.t.secret == nil {
		return nil
	}
	for _, p := range c.t.secret(row) {
		v, err := c.box.Decrypt(*p)
		if err != nil {
			return fmt.Errorf("%s: %w", c.t.name, err)
		}
		*p = v
	}
	return nil
}

func (c crud[T]) Create(ctx context.Context, row T) error {
	if err := c.seal(&row); err != nil {
		return err
	}
	_, err := c.q.NamedExecContext(ctx, c.t.insert, row)
	return mapErr(err)
}

func (c crud[T]) Get(ctx context.Context, id string) (T, error) {
	return c.one(ctx, "id = ?", id)
}

// Update writes every column of row, matched by id.
func (c crud[T]) Update(ctx context.Context, row T) error {
	if err := c.seal(&row); err != nil {
		return err
	}
	res, err := c.q.NamedExecContext(ctx, c.t.update, row)
	return affected(res, err)
}

func (c crud[T]) Delete(ctx context.Context, id string) error {
	res, err := c.q.ExecContext(ctx, "DELETE FROM "+c.t.name+" WHERE id = ?", id)
	return affected(res, err)
}

func (c crud[T]) one(ctx context.Context, where string, args ...any) (T, error) {
	var row T
	err := c.q.GetContext(ctx, &row, "SELECT "+c.t.cols+" FROM "+c.t.name+" WHERE "+where, args...)
	if err != nil {
		return row, mapErr(err)
	}
	return row, c.open(&row)
}

func (c crud[T]) many(ctx context.Context, where string, args ...any) ([]T, error) {
	var rows []T
	err := c.q.SelectContext(ctx, &rows, "SELECT "+c.t.cols+" FROM "+c.t.name+" WHERE "+where, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	for i := range rows {
		if err := c.open(&rows[i]); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// mapErr turns the driver's answers into the service vocabulary: a missing
// row is ErrNotFound, a unique clash is a Conflict.
func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errs.ErrNotFound
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %v", errs.Conflictf("already exists"), err)
		}
	}
	return err
}
