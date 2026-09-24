// Package servicetest is the test harness: a real SQLite database in
// t.TempDir() with migrations applied, the Docker fake wired in, and one
// *service.Orchestrator per test. Seed helpers write rows straight through
// the store so a test states its world in a few lines.
package servicetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/google/uuid"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Key is the master key every harness uses.
var Key = strings.Repeat("ab", 32)

type Env struct {
	O      *service.Orchestrator
	Docker *dockerfake.Fake
	Store  *store.Store // for seeding and for asserting rows
	Config service.Config
}

// New builds a fresh orchestrator. edit, if given, adjusts the config.
func New(t *testing.T, edit ...func(*service.Config)) *Env {
	t.Helper()
	dir := t.TempDir()
	cfg := service.Config{DataDir: dir, DBPath: filepath.Join(dir, "stackr.db"), SecretsKey: Key}
	for _, f := range edit {
		f(&cfg)
	}
	fake := dockerfake.New()
	o, err := service.New(cfg, service.WithDocker(fake))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })

	// A second pool on the same file: the orchestrator keeps its store to
	// itself, the harness gets its own.
	return &Env{O: o, Docker: fake, Store: open(t, cfg.DBPath), Config: cfg}
}

// Store is a fresh migrated store with no orchestrator, for tests of the
// layers below it (store, leaves, flows).
func Store(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stackr.db")
	st := open(t, path)
	if err := sqlite.Migrate(st.DB(), appdb.MigrateConfig()); err != nil {
		t.Fatal(err)
	}
	return st
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	db, err := sqlite.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var fk int
	if err := db.Get(&fk, "PRAGMA foreign_keys"); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", fk, err)
	}
	box, err := secrets.New(Key)
	if err != nil {
		t.Fatal(err)
	}
	return store.New(db, box)
}

var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// User seeds an active user and returns its id.
func (e *Env) User(t *testing.T, email string, admin bool) string {
	t.Helper()
	role := "user"
	if admin {
		role = "admin"
	}
	id := uuid.NewString()
	must(t, e.Store.Users.Create(context.Background(), store.User{
		ID: id, Email: email, PasswordHash: "x", Name: email, Role: role, Active: true,
		Theme: "system", CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

// Org seeds an org and returns its id.
func (e *Env) Org(t *testing.T, slug string) string {
	t.Helper()
	id := uuid.NewString()
	must(t, e.Store.Orgs.Create(context.Background(), store.Org{
		ID: id, Name: slug, Slug: slug, EnvColors: "{}", Settings: "{}", CreatedAt: now,
	}))
	return id
}

// Member seeds a membership.
func (e *Env) Member(t *testing.T, orgID, userID, role string) {
	t.Helper()
	must(t, e.Store.OrgMembers.Create(context.Background(), store.OrgMember{
		ID: uuid.NewString(), OrgID: orgID, UserID: userID, Role: role, CreatedAt: now,
	}))
}

// APIKey seeds a key (orgID "" = unbound) and returns its bearer token.
func (e *Env) APIKey(t *testing.T, userID, orgID string) string {
	t.Helper()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	var org *string
	if orgID != "" {
		org = &orgID
	}
	must(t, e.Store.APIKeys.Create(context.Background(), store.APIKey{
		ID: uuid.NewString(), UserID: userID, OrgID: org, Name: "test", TokenHash: user.HashToken(token), CreatedAt: now,
	}))
	return token
}

// Session opens a browser session for the user and returns its cookie token.
func (e *Env) Session(t *testing.T, userID string) string {
	t.Helper()
	s, err := e.O.Sessions().CreateSession(context.Background(), userID, nil)
	must(t, err)
	return s.Token
}
