package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/jmoiron/sqlx"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Config is everything service.New needs to build the service tree.
type Config struct {
	DataDir    string // job logs and other state live under it
	DBPath     string // "" = <DataDir>/stackr.db
	SecretsKey string // 64 hex chars; required, never generated here

	// Boot values for knobs the settings catalogue also holds: 0 = the
	// catalogue default; a settings row, once written, wins (DECIDE 15).
	Workers             int
	ImageWatchInterval  time.Duration
	OrphanRetentionDays int

	CookieSecure bool
	CookieDomain string
}

// Option changes how New builds the tree; tests use it.
type Option func(*options)

type options struct{ docker Docker }

// WithDocker replaces the daemon client, with the fake in tests.
func WithDocker(d Docker) Option { return func(o *options) { o.docker = d } }

// Orchestrator is the single door into the service tree: the only thing
// main, the API and the web handlers see. One method per user-facing verb.
type Orchestrator struct {
	db       *sqlx.DB
	store    *store.Store
	sessions *auth.SessionManager
	docker   Docker
	users    *user.Leaf
	orgs     *org.Leaf
	stacks   *stack.Leaf
	envs     *environment.Leaf
	tiles    *tile.Leaf
	settings *settings.Leaf
}

// onBuild is a test hook: every constructor New calls reports its name here,
// so a test can assert each runs once (B0).
var onBuild = func(string) {}

func build[T any](name string, f func() T) T {
	onBuild(name)
	return f()
}

// New builds the orchestrator and everything below it, once.
func New(cfg Config, opts ...Option) (*Orchestrator, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	box, err := secrets.New(cfg.SecretsKey)
	if err != nil {
		return nil, err
	}
	if cfg.DataDir == "" {
		return nil, errors.New("service: DataDir is required")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "stackr.db")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := sqlite.ConnectContext(ctx, cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := sqlite.Migrate(db, appdb.MigrateConfig()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if o.docker == nil {
		c, err := docker.New()
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		o.docker = c
	}

	st := build("store", func() *store.Store { return store.New(db, box) })
	return &Orchestrator{
		db:    db,
		store: st,
		sessions: build("sessions", func() *auth.SessionManager {
			return auth.NewSessionManager(st.Sessions,
				auth.WithCookieSecure(cfg.CookieSecure),
				auth.WithCookieDomain(cfg.CookieDomain))
		}),
		docker: o.docker,
		users:  build("leaf/user", func() *user.Leaf { return user.New(st.Users, st.Sessions, st.APIKeys) }),
		orgs:   build("leaf/org", func() *org.Leaf { return org.New(st.Orgs, st.OrgMembers) }),
		stacks: build("leaf/stack", func() *stack.Leaf { return stack.New(st.Stacks) }),
		envs:   build("leaf/environment", func() *environment.Leaf { return environment.New(st.Environments) }),
		tiles:  build("leaf/tile", func() *tile.Leaf { return tile.New(st.Tiles) }),
		settings: build("leaf/settings", func() *settings.Leaf {
			return settings.New(st.Settings, bootSettings(cfg))
		}),
	}, nil
}

// bootSettings turns the Config knobs that are set into settings values.
func bootSettings(cfg Config) map[string]string {
	boot := map[string]string{}
	if cfg.Workers > 0 {
		boot["workers"] = strconv.Itoa(cfg.Workers)
	}
	if cfg.ImageWatchInterval > 0 {
		boot["image_check_interval"] = strconv.Itoa(int(cfg.ImageWatchInterval / time.Minute))
	}
	if cfg.OrphanRetentionDays > 0 {
		boot["orphan_retention_days"] = strconv.Itoa(cfg.OrphanRetentionDays)
	}
	return boot
}

// Close releases the database.
func (o *Orchestrator) Close() error { return o.db.Close() }

// Ping reports whether the service can answer: the database is reachable.
func (o *Orchestrator) Ping(ctx context.Context) error {
	if err := o.store.Ping(ctx); err != nil {
		return fmt.Errorf("%w: database: %v", errs.ErrBusy, err)
	}
	return nil
}

// Sessions is hamr's session manager, for the web layer's cookie handling.
func (o *Orchestrator) Sessions() *auth.SessionManager { return o.sessions }
