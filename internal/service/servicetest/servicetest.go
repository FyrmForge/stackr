// Package servicetest is the test harness: a real SQLite database in
// t.TempDir() with migrations applied, the Docker fake wired in, and one
// *service.Orchestrator per test. Seed helpers write rows straight through
// the store so a test states its world in a few lines.
package servicetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// The fake stands in for the daemon everywhere.
var _ service.Docker = (*dockerfake.Fake)(nil)

// Key is the master key every harness uses.
var Key = storetest.Key

type Env struct {
	O      *service.Orchestrator
	Docker *dockerfake.Fake
	Store  *store.Store // for seeding and for asserting rows
	Config service.Config
}

// New builds a fresh orchestrator. edit, if given, adjusts the config.
func New(t *testing.T, edit ...func(*service.Config)) *Env {
	t.Helper()
	return NewWith(t, nil, edit...)
}

// NewWith is New with extra options (WithBuild) after the harness's own.
func NewWith(t *testing.T, opts []service.Option, edit ...func(*service.Config)) *Env {
	t.Helper()
	dir := t.TempDir()
	cfg := service.Config{DataDir: dir, DBPath: filepath.Join(dir, "stackr.db"), SecretsKey: Key}
	for _, f := range edit {
		f(&cfg)
	}
	fake := dockerfake.New()
	o, err := service.New(cfg, append([]service.Option{service.WithDocker(fake),
		service.WithProxy(func(context.Context, json.RawMessage) error { return nil })}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })

	// A second pool on the same file: the orchestrator keeps its store to
	// itself, the harness gets its own.
	return &Env{O: o, Docker: fake, Store: open(t, cfg.DBPath), Config: cfg}
}

// Store is a fresh migrated store with no orchestrator (storetest.Store).
func Store(t *testing.T) *store.Store { return storetest.Store(t) }

func open(t *testing.T, path string) *store.Store { return storetest.Open(t, path) }

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

// Tile is a seeded stack "shop", env "dev" and image tile "api" in an org.
type Tile struct{ Stack, Env, ID string }

// Tile seeds shop/dev/api in the org through the verbs.
func (e *Env) Tile(t *testing.T, orgID string) Tile {
	t.Helper()
	ctx := context.Background()
	st, err := e.O.CreateStack(ctx, orgID, "shop", "")
	must(t, err)
	en, err := e.O.CreateEnv(ctx, st.ID, "dev", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	tl, err := e.O.CreateTile(ctx, service.Tile{StackID: st.ID, EnvironmentID: en.ID, Name: "api", Kind: "image",
		ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	return Tile{Stack: st.ID, Env: en.ID, ID: tl.ID}
}

// Image seeds an image row and returns its id.
func (e *Env) Image(t *testing.T, ref string) string {
	t.Helper()
	id := uuid.NewString()
	must(t, e.Store.Images.Create(context.Background(), store.Image{ID: id, Ref: ref, BuiltAt: &now, CreatedAt: now}))
	return id
}

// Connector seeds a connected GitHub connector whose webhook secret is
// secret and returns its id.
func (e *Env) Connector(t *testing.T, orgID, secret string) string {
	t.Helper()
	id := uuid.NewString()
	cfg, err := json.Marshal(map[string]any{"app": map[string]any{"id": 1, "slug": "stackr-test", "webhook_secret": secret}})
	must(t, err)
	must(t, e.Store.Connectors.Create(context.Background(), store.Connector{
		ID: id, OrgID: orgID, Provider: "github", Name: "github", Host: "github.com", Config: string(cfg), CreatedAt: now,
	}))
	return id
}
