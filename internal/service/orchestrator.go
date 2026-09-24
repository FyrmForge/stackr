package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/jmoiron/sqlx"

	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/container"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/schedule"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/proxy"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// ProxyAdmin is where `stackrd proxy` serves Caddy's admin API, and the
// admin listener every pushed config carries (without it Caddy falls back to
// its own localhost and stackrd loses the proxy).
const ProxyAdmin = "0.0.0.0:2019"

// ProxyContainer is the proxy's container name on the box.
const ProxyContainer = "stackr-proxy"

// Config is everything service.New needs to build the service tree.
type Config struct {
	DataDir    string // job logs, clones, backups and other state live under it
	DBPath     string // "" = <DataDir>/stackr.db
	SecretsKey string // 64 hex chars; required, never generated here

	// Boot values for knobs the settings catalogue also holds: 0 = the
	// catalogue default; a settings row, once written, wins (DECIDE 15).
	Workers             int
	ImageWatchInterval  time.Duration
	OrphanRetentionDays int

	CookieSecure bool
	CookieDomain string

	Version       string // the running build: a release tag, or "dev"
	BaseURL       string // the panel's public URL (GitHub App callbacks)
	InstallID     string // names the panel archives
	Passphrase    string // age passphrase on the panel archive; "" = the secrets key
	TLSOff        bool   // STACKR_TLS=off: plain HTTP only
	PanelUpstream string // the panel's dial address for its own route; "" = stackr:8080
	// PanelSpec is the panel's container spec for an image, the installer's
	// one spec; nil refuses self-upgrade.
	PanelSpec func(image string) ContainerSpec
}

// Option changes how New builds the tree; tests use it.
type Option func(*options)

type options struct {
	docker Docker
	vip    tile.VIP
	push   func(context.Context, json.RawMessage) error
	build  BuildFunc
}

// BuildFunc builds one git tile at a commit and returns the image row id.
type BuildFunc func(ctx context.Context, st Stack, t Tile, commit string, log io.Writer) (string, error)

// WithBuild replaces the clone-and-build of git tiles, so a push lands a
// release without GitHub or a daemon.
func WithBuild(f BuildFunc) Option { return func(o *options) { o.build = f } }

// WithDocker replaces the daemon client, with the fake in tests.
func WithDocker(d Docker) Option { return func(o *options) { o.docker = d } }

// WithVIP replaces the iptables VIP table (it needs root and a netns).
func WithVIP(v tile.VIP) Option { return func(o *options) { o.vip = v } }

// WithProxy replaces the push to Caddy's admin API.
func WithProxy(push func(context.Context, json.RawMessage) error) Option {
	return func(o *options) { o.push = push }
}

// Orchestrator is the single door into the service tree: the only thing
// main, the API and the web handlers see. One method per user-facing verb.
type Orchestrator struct {
	cfg      Config
	db       *sqlx.DB
	store    *store.Store
	sessions *auth.SessionManager
	docker   Docker

	users    *user.Leaf
	orgs     *org.Leaf
	stacks   *stack.Leaf
	envs     *environment.Leaf
	tiles    *tile.Leaf
	images   *image.Leaf
	params   *params.Leaf
	volumes  *volume.Leaf
	domains  *domain.Leaf
	creds    *credential.Leaf
	conns    *connector.Leaf
	managed  *managed.Leaf
	releases *release.Leaf
	jobRows  *job.Leaf
	backups  *backup.Leaf
	settings *settings.Leaf

	deploy    *deploy.Flow
	engines   *mflow.Flow
	promote   *promote.Flow
	backup    *fbackup.Flow
	container *container.Flow
	watch     *imagewatch.Flow
	upgrade   *upgrade.Flow
	jobs      *jobs.Runner
	sched     *schedule.Runner
	sync      *domain.Syncer

	proxyStarted atomic.Value // the proxy container's last seen start time
	repoLocks    sync.Map     // clone dir -> *sync.Mutex
	cli          cliCodes
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
	if cfg.PanelUpstream == "" {
		// ponytail: the installer (step 5) gives the panel this network alias,
		// so the route survives the upgrade's stackr-<version> rename.
		cfg.PanelUpstream = "stackr:8080"
	}
	if cfg.Passphrase == "" {
		cfg.Passphrase = cfg.SecretsKey
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
	if o.vip == nil {
		o.vip = vip.New()
	}
	if o.push == nil {
		o.push = proxy.New("http://" + ProxyContainer + ":2019").Push
	}
	d := o.docker

	st := build("store", func() *store.Store { return store.New(db, box) })
	svc := &Orchestrator{cfg: cfg, db: db, store: st, docker: d}
	svc.sessions = build("sessions", func() *auth.SessionManager {
		return auth.NewSessionManager(st.Sessions,
			auth.WithCookieSecure(cfg.CookieSecure),
			auth.WithCookieDomain(cfg.CookieDomain))
	})
	svc.users = build("leaf/user", func() *user.Leaf { return user.New(st.Users, st.Sessions, st.APIKeys) })
	svc.orgs = build("leaf/org", func() *org.Leaf { return org.New(st.Orgs, st.OrgMembers, st.Invites) })
	svc.stacks = build("leaf/stack", func() *stack.Leaf { return stack.New(st.Stacks) })
	svc.envs = build("leaf/environment", func() *environment.Leaf { return environment.New(st.Environments, d) })
	svc.tiles = build("leaf/tile", func() *tile.Leaf { return tile.New(st.Tiles, d, o.vip) })
	svc.images = build("leaf/image", func() *image.Leaf { return image.New(st.Images, d) })
	svc.params = build("leaf/params", func() *params.Leaf { return params.New(st.Params) })
	svc.volumes = build("leaf/volume", func() *volume.Leaf { return volume.New(st.Volumes, d) })
	svc.domains = build("leaf/domain", func() *domain.Leaf { return domain.New(st.Domains, d, ProxyContainer) })
	svc.creds = build("leaf/credential", func() *credential.Leaf { return credential.New(st.Credentials) })
	svc.conns = build("leaf/connector", func() *connector.Leaf {
		return connector.New(st.Connectors, githubapp.New(cfg.BaseURL))
	})
	svc.managed = build("leaf/managed", func() *managed.Leaf { return managed.New(st.ManagedInstances, st.Provisions) })
	svc.releases = build("leaf/release", func() *release.Leaf { return release.New(st.Releases, st.ReleaseTiles) })
	svc.jobRows = build("leaf/job", func() *job.Leaf { return job.New(st.Jobs) })
	svc.backups = build("leaf/backup", func() *backup.Leaf { return backup.New(st.BackupDests, st.BackupSchedules, st.BackupRuns) })
	svc.settings = build("leaf/settings", func() *settings.Leaf { return settings.New(st.Settings, bootSettings(cfg)) })

	svc.engines = build("flow/managed", func() *mflow.Flow {
		return &mflow.Flow{Tiles: svc.tiles, Instances: svc.managed, Volumes: svc.volumes, Envs: svc.envs,
			S3: func(endpoint, access, secret string) mflow.S3Admin {
				return s3.Admin{Endpoint: endpoint, AccessKey: access, SecretKey: secret}
			}}
	})
	svc.sync = build("leaf/domain.Syncer", func() *domain.Syncer {
		return &domain.Syncer{Build: svc.proxyConfig, Push: o.push}
	})
	svc.deploy = build("flow/deploy", func() *deploy.Flow {
		return &deploy.Flow{Tiles: svc.tiles, Envs: svc.envs, Stacks: svc.stacks, Orgs: svc.orgs,
			Volumes: svc.volumes, Images: svc.images, Releases: svc.releases, Params: svc.params,
			Managed: svc.managed, Domains: svc.domains, Creds: svc.creds, Settings: svc.settings,
			Jobs: svc.jobRows, Sync: svc.sync.Sync, Engines: svc.engines}
	})
	svc.promote = build("flow/promote", func() *promote.Flow {
		b := svc.buildTile
		if o.build != nil {
			b = o.build
		}
		return &promote.Flow{D: svc.deploy, Config: svc.stackFile, Build: b,
			DNS01: svc.dns01}
	})
	svc.backup = build("flow/backup", func() *fbackup.Flow {
		return &fbackup.Flow{Backups: svc.backups, Volumes: svc.volumes, Tiles: svc.tiles,
			Scratch: filepath.Join(cfg.DataDir, "backups", "scratch")}
	})
	svc.container = build("flow/container", func() *container.Flow { return &container.Flow{Tiles: svc.tiles} })
	svc.watch = build("flow/imagewatch", func() *imagewatch.Flow {
		return &imagewatch.Flow{Orgs: svc.orgs, Stacks: svc.stacks, Envs: svc.envs, Tiles: svc.tiles,
			Images: svc.images, Releases: svc.releases, Creds: svc.creds, Settings: svc.settings}
	})
	svc.upgrade = build("flow/upgrade", func() *upgrade.Flow {
		return &upgrade.Flow{Panel: panel.New(d), Version: cfg.Version, Archive: svc.upgradeArchive, Spec: svc.panelSpec}
	})
	svc.jobs = build("flow/jobs", func() *jobs.Runner {
		// ponytail: no ParamSet, a parked job is requeued every poll and its
		// handler re-checks (DECIDE 17 (b)).
		return jobs.New(svc.jobRows, svc.handlers(), cfg.DataDir, jobs.Options{
			Workers: func(ctx context.Context) (int, error) { return svc.settings.Int(ctx, "workers") },
		})
	})
	svc.sched = build("flow/schedule", func() *schedule.Runner {
		return schedule.New(schedule.Drivers{
			Schedules: svc.backups.AllSchedules,
			Backup: func(ctx context.Context, s store.BackupSchedule) error {
				_, err := svc.enqueue(ctx, kindBackup, backupJob{ScheduleID: s.ID, VolumeID: s.VolumeID}, "volume:"+s.VolumeID)
				return err
			},
			Orphans: func(ctx context.Context) error { _, err := svc.enqueue(ctx, kindOrphans, nil, "orphans"); return err },
			Watch:   svc.watchTick,
		})
	})

	if _, err := svc.localDest(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("local backup destination: %w", err)
	}
	if err := svc.jobs.Start(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("start job runner: %w", err)
	}
	if err := svc.sched.Boot(ctx); err != nil {
		// A bad schedule row is reported, never fatal: the rest still run.
		slog.Warn("scheduler: entries skipped", "err", err)
	}
	go func() {
		if err := svc.sync.Sync(context.Background()); err != nil {
			slog.Warn("proxy: boot push failed", "err", err)
		}
	}()
	return svc, nil
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

// Close stops the scheduler and the job runner, then releases the database.
func (o *Orchestrator) Close() error {
	o.sched.Stop()
	o.jobs.Close()
	return o.db.Close()
}

// Ping reports whether the service can answer: the database is reachable.
func (o *Orchestrator) Ping(ctx context.Context) error {
	if err := o.store.Ping(ctx); err != nil {
		return fmt.Errorf("%w: database: %v", errs.ErrBusy, err)
	}
	return nil
}

// Sessions is hamr's session manager, for the web layer's cookie handling.
func (o *Orchestrator) Sessions() *auth.SessionManager { return o.sessions }
