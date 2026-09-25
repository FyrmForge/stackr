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
	"github.com/FyrmForge/stackr/internal/service/internal/flow/graph"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/imagewatch"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	frun "github.com/FyrmForge/stackr/internal/service/internal/flow/run"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/schedule"
	ftraffic "github.com/FyrmForge/stackr/internal/service/internal/flow/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/upgrade"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
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
	// The installer's answers (env on the panel container), same rule.
	PanelDomain    string
	RootDomain     string // tiles get names under it; seeds the instance domain resource
	ACMEEmail      string
	TrustedProxies string // CIDRs Caddy trusts X-Forwarded-For from
	DNSProvider    string // "cloudflare" = DNS-01, the token sits on the proxy

	CookieSecure bool
	CookieDomain string

	Version       string // the running build: a release tag, or "dev"
	BaseURL       string // the panel's public URL (GitHub App callbacks)
	InstallID     string // names the panel archives
	Passphrase    string // age passphrase on the panel archive; "" = the secrets key
	TLSOff        bool   // STACKR_TLS=off: plain HTTP only
	PanelUpstream string // the panel's dial address for its own route; "" = stackr:8080
	ProxyAdmin    string // Caddy's admin API as the panel reaches it; "" = http://stackr-proxy:2019
	// PanelSpec is the panel's container spec for an image, the installer's
	// one spec; nil refuses self-upgrade.
	PanelSpec func(image string) ContainerSpec
	// Conntrack is the host conntrack table the traffic sample reads;
	// "" = /proc/net/nf_conntrack (the panel is host-network).
	Conntrack string
}

// Option changes how New builds the tree; tests use it.
type Option func(*options)

type options struct {
	docker Docker
	vip    tile.VIP
	push   func(context.Context, json.RawMessage) error
	build  BuildFunc
	gitEnv []string
}

// BuildFunc builds one git tile at a commit and returns the image row id.
type BuildFunc func(ctx context.Context, st Stack, t Tile, commit string, log io.Writer) (string, error)

// WithBuild replaces the clone-and-build of git tiles, so a push lands a
// release without GitHub or a daemon.
func WithBuild(f BuildFunc) Option {
	return func(o *options) { o.build = f }
}

// WithGit clones with env instead of a connector's token: no connector
// lookup, no GitHub. Tests point https://github.com/ at local bare repos
// with it (servicetest.Git).
func WithGit(env ...string) Option {
	return func(o *options) { o.gitEnv = env }
}

// WithDocker replaces the daemon client, with the fake in tests.
func WithDocker(d Docker) Option {
	return func(o *options) { o.docker = d }
}

// WithVIP replaces the iptables VIP table (it needs root and a netns).
func WithVIP(v tile.VIP) Option {
	return func(o *options) { o.vip = v }
}

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

	users     *user.Leaf
	orgs      *org.Leaf
	orgPlans  *orgplan.Leaf
	stacks    *stack.Leaf
	envs      *environment.Leaf
	tiles     *tile.Leaf
	images    *image.Leaf
	params    *params.Leaf
	volumes   *volume.Leaf
	domains   *domain.Leaf
	domainres *domainres.Leaf
	creds     *credential.Leaf
	conns     *connector.Leaf
	managed   *managed.Leaf
	releases  *release.Leaf
	jobRows   *job.Leaf
	backups   *backup.Leaf
	settings  *settings.Leaf
	runs      *lrun.Leaf
	traffic   *ltraffic.Leaf
	canvas    *canvas.Leaf

	deploy    *deploy.Flow
	engines   *mflow.Flow
	promote   *promote.Flow
	backup    *fbackup.Flow
	container *container.Flow
	watch     *imagewatch.Flow
	upgrade   *upgrade.Flow
	run       *frun.Flow
	sample    *ftraffic.Flow
	graph     *graph.Flow
	jobs      *jobs.Runner
	sched     *schedule.Runner
	sync      *domain.Syncer

	gitEnv       []string     // WithGit: clone env that replaces the connector's
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
	if cfg.Conntrack == "" {
		cfg.Conntrack = ftraffic.DefaultPath
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
		admin := cfg.ProxyAdmin
		if admin == "" {
			admin = "http://" + ProxyContainer + ":2019"
		}
		o.push = proxy.New(admin).Push
	}
	d := o.docker

	st := build("store", func() *store.Store { return store.New(db, box) })
	orch := &Orchestrator{
		cfg:    cfg,
		db:     db,
		store:  st,
		docker: d,
		gitEnv: o.gitEnv,
	}
	orch.sessions = build("sessions", func() *auth.SessionManager {
		return auth.NewSessionManager(st.Sessions,
			auth.WithCookieSecure(cfg.CookieSecure),
			auth.WithCookieDomain(cfg.CookieDomain))
	})
	orch.users = build("leaf/user", func() *user.Leaf { return user.New(st.Users, st.Sessions, st.APIKeys) })
	orch.orgs = build("leaf/org", func() *org.Leaf { return org.New(st.Orgs, st.OrgMembers, st.Invites) })
	orch.orgPlans = build("leaf/orgplan", func() *orgplan.Leaf { return orgplan.New(st.OrgPlans) })
	orch.stacks = build("leaf/stack", func() *stack.Leaf { return stack.New(st.Stacks) })
	orch.envs = build("leaf/environment", func() *environment.Leaf { return environment.New(st.Environments, d) })
	orch.tiles = build("leaf/tile", func() *tile.Leaf { return tile.New(st.Tiles, d, o.vip) })
	orch.images = build("leaf/image", func() *image.Leaf { return image.New(st.Images, d) })
	orch.params = build("leaf/params", func() *params.Leaf { return params.New(st.Params) })
	orch.volumes = build("leaf/volume", func() *volume.Leaf { return volume.New(st.Volumes, d) })
	orch.domains = build("leaf/domain", func() *domain.Leaf { return domain.New(st.Domains, d, ProxyContainer) })
	orch.domainres = build("leaf/domainres", func() *domainres.Leaf { return domainres.New(st.DomainResources) })
	orch.creds = build("leaf/credential", func() *credential.Leaf { return credential.New(st.Credentials) })
	orch.conns = build("leaf/connector", func() *connector.Leaf {
		return connector.New(st.Connectors, githubapp.New(cfg.BaseURL))
	})
	orch.managed = build("leaf/managed",
		func() *managed.Leaf { return managed.New(st.ManagedInstances, st.Provisions, st.Bindings) })
	orch.releases = build("leaf/release", func() *release.Leaf { return release.New(st.Releases, st.ReleaseTiles) })
	orch.jobRows = build("leaf/job", func() *job.Leaf { return job.New(st.Jobs) })
	orch.backups = build("leaf/backup",
		func() *backup.Leaf { return backup.New(st.BackupDests, st.BackupSchedules, st.BackupRuns) })
	orch.settings = build("leaf/settings",
		func() *settings.Leaf { return settings.New(st.Settings, bootSettings(cfg)) })
	orch.runs = build("leaf/run", func() *lrun.Leaf { return lrun.New(st.Runs, filepath.Join(cfg.DataDir, "runs")) })
	orch.traffic = build("leaf/traffic", ltraffic.New)
	orch.canvas = build("leaf/canvas", func() *canvas.Leaf { return canvas.New(st.Positions, st.Annotations) })

	orch.engines = build("flow/managed", func() *mflow.Flow {
		return &mflow.Flow{
			Tiles:     orch.tiles,
			Instances: orch.managed,
			Volumes:   orch.volumes,
			Envs:      orch.envs,
			S3: func(endpoint, access, secret string) mflow.S3Admin {
				return s3.Admin{Endpoint: endpoint, AccessKey: access, SecretKey: secret}
			},
			PublicBase: orch.publicBase,
		}
	})
	orch.sync = build("leaf/domain.Syncer", func() *domain.Syncer {
		return &domain.Syncer{Build: orch.proxyConfig, Push: o.push}
	})
	orch.deploy = build("flow/deploy", func() *deploy.Flow {
		return &deploy.Flow{
			Tiles:    orch.tiles,
			Envs:     orch.envs,
			Stacks:   orch.stacks,
			Orgs:     orch.orgs,
			Volumes:  orch.volumes,
			Images:   orch.images,
			Releases: orch.releases,
			Params:   orch.params,
			Managed:  orch.managed,
			Domains:  orch.domains,
			Creds:    orch.creds,
			Settings: orch.settings,
			Jobs:     orch.jobRows,
			Sync:     orch.sync.Sync,
			Engines:  orch.engines,
		}
	})
	orch.promote = build("flow/promote", func() *promote.Flow {
		b := orch.buildTile
		if o.build != nil {
			b = o.build
		}
		return &promote.Flow{
			D:         orch.deploy,
			Resources: orch.domainres,
			Config:    orch.stackFile,
			Build:     b,
			DNS01:     orch.dns01,
		}
	})
	orch.backup = build("flow/backup", func() *fbackup.Flow {
		return &fbackup.Flow{
			Backups: orch.backups,
			Volumes: orch.volumes,
			Tiles:   orch.tiles,
			Scratch: filepath.Join(cfg.DataDir, "backups", "scratch"),
		}
	})
	orch.container = build("flow/container", func() *container.Flow { return &container.Flow{Tiles: orch.tiles} })
	orch.watch = build("flow/imagewatch", func() *imagewatch.Flow {
		return &imagewatch.Flow{
			Orgs:     orch.orgs,
			Stacks:   orch.stacks,
			Envs:     orch.envs,
			Tiles:    orch.tiles,
			Images:   orch.images,
			Releases: orch.releases,
			Creds:    orch.creds,
			Settings: orch.settings,
		}
	})
	orch.upgrade = build("flow/upgrade", func() *upgrade.Flow {
		return &upgrade.Flow{
			Panel:   panel.New(d),
			Version: cfg.Version,
			Archive: orch.upgradeArchive,
			Spec:    orch.panelSpec,
		}
	})
	orch.run = build("flow/run", func() *frun.Flow {
		return &frun.Flow{
			Tiles:  orch.tiles,
			Envs:   orch.envs,
			Runs:   orch.runs,
			Jobs:   orch.jobRows,
			Deploy: orch.deploy,
		}
	})
	orch.sample = build("flow/traffic", func() *ftraffic.Flow {
		return &ftraffic.Flow{
			Tiles:   orch.tiles,
			Envs:    orch.envs,
			Domains: orch.domains,
			Managed: orch.managed,
			Traffic: orch.traffic,
			Path:    cfg.Conntrack,
		}
	})
	orch.graph = build("flow/graph", func() *graph.Flow {
		return &graph.Flow{
			Orgs:     orch.orgs,
			Stacks:   orch.stacks,
			Envs:     orch.envs,
			Tiles:    orch.tiles,
			Params:   orch.params,
			Volumes:  orch.volumes,
			Domains:  orch.domains,
			Managed:  orch.managed,
			Conns:    orch.conns,
			Releases: orch.releases,
			Jobs:     orch.jobRows,
			Runs:     orch.runs,
			Canvas:   orch.canvas,
			Images:   orch.images,
		}
	})
	orch.jobs = build("flow/jobs", func() *jobs.Runner {
		// ponytail: no ParamSet, a parked job is requeued every poll and its
		// handler re-checks (DECIDE 17 (b)).
		return jobs.New(orch.jobRows, orch.handlers(), cfg.DataDir, jobs.Options{
			Workers: func(ctx context.Context) (int, error) { return orch.settings.Int(ctx, "workers") },
			// A run holds its own clock: the tile's timeout_minutes.
			Uncapped: map[jobs.Kind]bool{kindRun: true},
		})
	})
	orch.sched = build("flow/schedule", func() *schedule.Runner {
		return schedule.New(schedule.Drivers{
			Schedules: orch.backups.AllSchedules,
			Backup: func(ctx context.Context, s store.BackupSchedule) error {
				_, err := orch.enqueue(
					ctx,
					kindBackup,
					backupJob{ScheduleID: s.ID, VolumeID: s.VolumeID},
					"volume:"+s.VolumeID,
				)
				return err
			},
			Orphans: func(ctx context.Context) error {
				_, err := orch.enqueue(ctx, kindOrphans, nil, "orphans")
				return err
			},
			Watch: orch.watchTick,
			Crons: orch.deployedCrons,
			Cron: func(ctx context.Context, t store.Tile) error {
				_, _, err := orch.queueRun(ctx, t.ID, lrun.Schedule)
				return err
			},
			Traffic: orch.sample.Tick,
		})
	})

	if _, err := orch.localDest(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("local backup destination: %w", err)
	}
	if err := orch.runs.Interrupted(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("close interrupted runs: %w", err)
	}
	root, err := orch.settings.Get(ctx, "root_domain")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("root domain: %w", err)
	}
	if err := orch.domainres.SeedInstance(ctx, root); err != nil {
		// A root that will not parse, or one a resource already holds, is
		// reported, never fatal: the panel still serves.
		slog.Warn("domains: instance resource not seeded", "root", root, "err", err)
	}
	if err := orch.jobs.Start(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("start job runner: %w", err)
	}
	if err := orch.sched.Boot(ctx); err != nil {
		// A bad schedule row is reported, never fatal: the rest still run.
		slog.Warn("scheduler: entries skipped", "err", err)
	}
	if w := ftraffic.Check(cfg.Conntrack); w != "" {
		slog.Warn(w, "path", cfg.Conntrack)
	}
	go func() {
		if err := orch.domains.ReopenIngress(context.Background()); err != nil {
			slog.Warn("proxy: ingress rejoin failed", "err", err)
		}
		if err := orch.sync.Sync(context.Background()); err != nil {
			slog.Warn("proxy: boot push failed", "err", err)
		}
	}()
	return orch, nil
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
	for k, v := range map[string]string{
		"panel_domain":    cfg.PanelDomain,
		"root_domain":     cfg.RootDomain,
		"acme_email":      cfg.ACMEEmail,
		"trusted_proxies": cfg.TrustedProxies,
		"dns_provider":    cfg.DNSProvider,
	} {
		if v != "" {
			boot[k] = v
		}
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
