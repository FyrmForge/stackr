package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
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
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors/all"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/fakehost"
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
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/canvas"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/orgplan"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/route"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tier"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	ltraffic "github.com/FyrmForge/stackr/internal/service/internal/leaf/traffic"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/user"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
	"github.com/FyrmForge/stackr/internal/service/internal/proxy"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
	"github.com/FyrmForge/stackr/internal/wgapply"
	"github.com/FyrmForge/stackr/internal/worker"
)

// ProxyAdmin is where `stackrd proxy` serves Caddy's admin API, and the
// admin listener every pushed config carries (without it Caddy falls back to
// its own localhost and stackrd loses the proxy).
const ProxyAdmin = installspec.ProxyAdminListen

// ProxyContainer is the proxy's container name on the box.
const ProxyContainer = "stackr-proxy"

// Config is everything service.New needs to build the service tree.
type Config struct {
	DataDir    string // job logs, clones, backups and other state live under it
	DBPath     string // "" = <DataDir>/stackr.db
	SecretsKey string // 64 hex chars; required, never generated here
	// KeyFromEnv is true when SecretsKey came from STACKR_MASTER_KEY: the
	// panel cannot rotate a key it does not own.
	KeyFromEnv bool
	// Restart ends the process so the supervisor starts it again; a
	// master key rotation takes effect on that boot. nil = nothing restarts.
	Restart func()

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

	CookieSecure bool
	CookieDomain string

	Version       string // the running build: a release tag, or "dev"
	BaseURL       string // the panel's public URL (provider callbacks)
	InstallID     string // names the panel archives
	TLSOff        bool   // STACKR_TLS=off: plain HTTP only
	PanelUpstream string // the panel's dial address for its own route; "" = stackr:8080
	ProxyAdmin    string // Caddy's admin API as the panel reaches it; "" = http://stackr-proxy:2019
	// PanelSpec is the panel's container spec for an image, the installer's
	// one spec; nil refuses self-upgrade.
	PanelSpec func(image string) ContainerSpec
	// ProxySpec is stackr-proxy's spec for an image, the same installer
	// spec; an upgrade recreates a stale proxy from it.
	ProxySpec func(image string) ContainerSpec
	// Conntrack is the host conntrack table the traffic sample reads;
	// "" = /proc/net/nf_conntrack (the panel is host-network).
	Conntrack string
	// PanelBind is the address the host-network panel listens on (the
	// installer's HOST: docker0's gateway); the filter rules let only Caddy
	// reach it. "" = no panel rule.
	PanelBind string
	// PanelPort is the port the panel really listens on (its PORT).
	PanelPort int
	// FakeHost runs on a simulated Docker, VIP table and conntrack whose
	// state lives in this directory (STACKR_FAKE_HOST, local development
	// only). It forces TLSOff: the local proxy never asks for certificates.
	FakeHost string
	// MeshWG and MeshRunner put the controller's own node in the mesh: the
	// kernel wg applier and the command runner of a real host. Nil MeshWG
	// (a fake host, a test) leaves the controller out; it then runs no mesh.
	MeshWG     wgapply.Applier
	MeshRunner worker.Runner
	// MeshWGPort is the controller's mesh UDP port (STACKR_MESH_WG_PORT,
	// the installer's --wg-port); 0 = mesh.DefaultPort.
	MeshWGPort int
	// RegistryImports sends every outside image a deploy runs through the
	// cluster registry (imports.go). Off for a fake host and for tests with no
	// registry behind them.
	RegistryImports bool
	// ProbeDests makes a backup destination pass a test write before it is
	// saved. Off for tests with no bucket behind them.
	ProbeDests bool
	// HostRoot is where the panel container sees the host's /proc and
	// /sys/fs/cgroup (STACKR_HOST_ROOT; "" = this process's own).
	HostRoot string
	// Metrics is the metrics store; nil = in memory.
	Metrics metrics.Store
}

// Option changes how New builds the tree; tests use it.
type Option func(*options)

type options struct {
	docker Docker
	vip    tile.VIP
	push   func(context.Context, json.RawMessage) error
	// caddyMetrics reads the controller's Caddy /metrics; nil with WithProxy.
	caddyMetrics func(context.Context) ([]byte, error)
	build        BuildFunc
	gitEnv       []string
	dnsOpen      func(kind string, c DNSCreds) (DNSProvider, error)
	kinds        []connectors.Kind
	ifaces       func() []worker.Iface
}

// DNSProvider is one DNS account (the DNS capability): what WithDNS fakes.
// DNSCreds is a connection's settings as the fake sees them.
type (
	DNSProvider = dnscap.Provider
	DNSZone     = dnscap.Zone
	DNSCreds    = map[string]string
)

// WithDNS replaces every DNS kind's provider calls with a fake in tests;
// the kind's own setup checks still run.
func WithDNS(open func(kind string, c DNSCreds) (DNSProvider, error)) Option {
	return func(o *options) { o.dnsOpen = open }
}

// WithHostInterfaces replaces the read of this host's network interfaces,
// so a test's fake controller never reports the machine it runs on.
func WithHostInterfaces(f func() []worker.Iface) Option {
	return func(o *options) { o.ifaces = f }
}

// WithKinds registers connector kinds besides the built-in ones (a test's
// fake kind).
func WithKinds(ks ...connectors.Kind) Option {
	return func(o *options) { o.kinds = append(o.kinds, ks...) }
}

// BuildFunc builds one git tile at a commit and returns the image row id.
type BuildFunc func(ctx context.Context, st Stack, t Tile, commit string, log io.Writer) (string, error)

// WithBuild replaces the clone-and-build of git tiles, so a push lands a
// release without a provider or a daemon.
func WithBuild(f BuildFunc) Option {
	return func(o *options) { o.build = f }
}

// WithGit clones with env instead of a connection's clone access: no
// connector lookup. Tests point a host at local bare repos with it
// (servicetest.Git).
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
	cfg Config
	// recMu guards the recovery key's public half and generation, which a
	// rotation changes while backups read them (backup.go).
	recMu  sync.Mutex
	recPub string
	recGen int
	// recoverSeen are the nonces of recent recovery calls (RecoverNode).
	recoverSeen recoverNonces
	db          *sqlx.DB
	store       *store.Store
	sessions    *auth.SessionManager
	docker      Docker
	vip         tile.VIP  // the boot rebuild reads it (vipboot.go)
	dnsKick     *coalesce // the records sync after a proxy push (dnssync.go)
	dnsSync     dnsState

	users       *user.Leaf
	orgs        *org.Leaf
	orgPlans    *orgplan.Leaf
	serverPlans *orgplan.Server
	stacks      *stack.Leaf
	envs        *environment.Leaf
	tiers       *tier.Leaf
	tiles       *tile.Leaf
	images      *image.Leaf
	params      *params.Leaf
	volumes     *volume.Leaf
	domains     *domain.Leaf
	domainres   *domainres.Leaf
	routes      *route.Leaf
	hostgrant   *hostgrant.Leaf
	creds       *credential.Leaf
	regs        *registry.Leaf
	regCleaned  lastRegistryClean
	conns       *connector.Leaf
	dns         *dnscap.Leaf
	kinds       []connectors.Kind // registered connector kinds (connectors/all)
	dnsOpen     func(kind string, c DNSCreds) (DNSProvider, error)
	managed     *managed.Leaf
	releases    *release.Leaf
	jobRows     *job.Leaf
	backups     *backup.Leaf
	settings    *settings.Leaf
	runs        *lrun.Leaf
	traffic     *ltraffic.Leaf
	canvas      *canvas.Leaf

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

	hostIfaces   func() []worker.Iface // this host's interfaces (WithHostInterfaces)
	gitEnv       []string              // WithGit: clone env that replaces the connector's
	proxyStarted atomic.Value          // the proxy container's last seen start time
	vipAsync     sync.WaitGroup        // rerouteVIPsAsync runs in flight; tests wait on it
	trustAsync   sync.WaitGroup        // detached rollover / controller cert steps (detach); Close waits
	vipLocks     sync.Map              // node ID -> *sync.Mutex: one VIP rebuild per node at a time
	earlyMu      sync.Mutex            // guards earlyIPs
	earlyIPs     map[string]earlyEnt   // container id -> address holding early lan rules
	repoLocks    sync.Map              // clone dir -> *sync.Mutex
	alertMem     *alertState           // the alert tick's memory (alerts.go)
	platforms    platformCache         // each node's CPU type (imports.go)
	cli          cliCodes

	cluster   *clusterPKI   // the CA, the controller cert, the worker clients (nodeclients.go)
	stopNodes chan struct{} // ends watchNodeClients
	// alerts is the alert queue (alerts.go); alertsDone waits for its
	// delivery goroutine.
	alerts     chan Alert
	alertsDone sync.WaitGroup
	// helperReady puts the mesh's helper image in the cluster registry
	// (rsyncSteps); tests swap it.
	helperReady func(context.Context) error
	mesh        *meshSvc // the node mesh and exits (mesh.go, multinode.go)
	stopMesh    context.CancelFunc
	runMesh     func() // starts the mesh loop once New has built every leaf
	meshCtx     context.Context
	// stackrPublished closes once the boot publish put the running stackr
	// image in the cluster registry (or gave up): an ingress worker's proxy
	// pull waits on it after an upgrade (systemimages.go).
	stackrPublished chan struct{}
	stackrOnce      sync.Once
	// upgradeLaunched is when this process launched the helper that swaps it
	// for a new panel (unix nanos, 0 = never): process memory, so the new
	// panel starts without it (UpgradeUnderway).
	upgradeLaunched atomic.Int64
	metrics         metricsState
	observe         observeState                          // stackr-observe, metrics history (observe.go)
	logship         logShip                               // container, access and run logs to the log store (logship.go)
	caddyMetrics    func(context.Context) ([]byte, error) // the controller's Caddy /metrics, nil in tests
	localWorker     *worker.Server                        // the controller's own node in the mesh, nil on a fake host
	rotation        rotation                              // mesh key rotation (rotate.go)
	stopOnce        sync.Once
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
	// A relative path is a volume name to Docker when it lands in a bind.
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
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
	if cfg.FakeHost != "" {
		fh, err := fakehost.New(cfg.FakeHost)
		if err != nil {
			return nil, err
		}
		if o.docker == nil {
			o.docker = fh
		}
		if o.vip == nil {
			o.vip = fakehost.VIP{}
		}
		if cfg.Conntrack == "" {
			if cfg.Conntrack, err = fh.Conntrack(); err != nil {
				return nil, err
			}
		}
		cfg.TLSOff = true
		slog.Warn("fake host: Docker, VIP and conntrack are simulated; local development only", "dir", cfg.FakeHost)
	}
	if cfg.Conntrack == "" {
		cfg.Conntrack = ftraffic.DefaultPath
	}
	recPub, recGen, err := ensureRecoveryKey(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("recovery key: %w", err)
	}
	// The migrations take no context, so the boot's budget starts after them:
	// on a slow disk they could spend it all and fail the boot, migrated, on
	// its first query.
	db, err := openDB(context.Background(), cfg.DBPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

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
		pc := proxy.New(admin)
		o.push, o.caddyMetrics = pc.Push, pc.Metrics
	}
	d := o.docker

	st := build("store", func() *store.Store { return store.New(db, box) })
	if err := ensureCanary(ctx, st); err != nil {
		_ = db.Close()
		return nil, err
	}
	nodes := build("leaf/node", func() *node.Leaf { return node.New(st.Nodes) })
	// The controller row is made once; "the head" is that row, never this process.
	ctl, created, err := nodes.Bootstrap(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("controller node: %w", err)
	}
	// a database migrated before the controller row existed: default stands on it
	if err := adoptDefault(ctx, st, ctl.ID); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("controller node: default placement: %w", err)
	}
	// envs from before nodes existed run on the controller
	if n, err := st.Environments.AdoptUnplaced(ctx, ctl.ID); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("controller node: adopt envs: %w", err)
	} else if n > 0 {
		slog.Info("nodes: envs placed on the controller", "count", n)
	}
	if created {
		if err := st.Tx(ctx, func(tx store.Tx) error {
			return audit(tx).Append(ctx, securitylog.Event{
				Action: securitylog.NodeBootstrap, TargetKind: "node", TargetID: ctl.ID,
			})
		}); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("controller node: %w", err)
		}
	}
	cl, err := bootCluster(ctx, st, cfg.DataDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, routed := d.(*DockerRouter); !routed { // WithDocker may bring its own router
		d = NewRouter(map[string]Docker{ctl.ID: d})
	}
	o.vip = newNodeVIPs(map[string]tile.VIP{ctl.ID: o.vip})
	if o.ifaces == nil {
		o.ifaces = worker.Interfaces
	}
	orch := &Orchestrator{
		recPub:     recPub,
		recGen:     recGen,
		hostIfaces: o.ifaces,
		dnsKick:    &coalesce{},
		alertMem:   &alertState{restarts: map[string][]restartSample{}, memLowSince: map[string]time.Time{}},
		cfg:        cfg,
		db:         db,
		store:      st,
		docker:     d,
		vip:        o.vip,
		gitEnv:     o.gitEnv,

		caddyMetrics: o.caddyMetrics,

		cluster:   cl,
		stopNodes: make(chan struct{}),
		alerts:    make(chan Alert, 64),

		stackrPublished: make(chan struct{}),
	}
	orch.dnsKick.fn = orch.syncDNS
	if nv, ok := orch.vip.(*nodeVIPs); ok {
		nv.down = orch.nodeDown
	}
	// Every network comes from one cluster pool: a reservation avoids every
	// env's network and every node's reported bridges, not just its own node's.
	nodes.Taken = func(ctx context.Context) ([]netip.Prefix, error) {
		if m := orch.mesh; m != nil {
			return m.taken(ctx)
		}
		return (&meshSvc{st: st}).taken(ctx) // single node: the env rows alone
	}
	orch.helperReady = orch.rsyncSteps().ensure
	if r, ok := d.(*DockerRouter); ok {
		r.OwnRegistry = orch.RegistryHost
		if c, ok := r.clients[ctl.ID].(*docker.Client); ok {
			c.SetClusterSource(orch.clusterImage)
		}
		r.Gate = orch.gate
		r.ToolReady = func(ctx context.Context, nodeID string) error {
			if nodeID == ctl.ID {
				return nil // the controller pulls ToolImage from its own registry
			}
			return orch.helperReady(ctx)
		}
	}
	// Before the first worker probe: a worker still on the previous build is
	// allowed only once panel_prev_version names it (the too-old rule).
	if err := orch.recordPanelVersion(ctx); err != nil {
		slog.Warn("panel version: not recorded", "err", err)
	}
	orch.startMesh(ctl.ID)
	if err := orch.refreshNodeClients(ctx); err != nil {
		slog.Warn("nodes: worker clients not all built", "err", err)
	}
	go orch.watchNodeClients()
	orch.sessions = build("sessions", func() *auth.SessionManager {
		return auth.NewSessionManager(st.Sessions,
			auth.WithCookieSecure(cfg.CookieSecure),
			auth.WithCookieDomain(cfg.CookieDomain))
	})
	orch.users = build("leaf/user", func() *user.Leaf { return user.New(st.Users, st.Sessions, st.APIKeys) })
	orch.orgs = build("leaf/org", func() *org.Leaf { return org.New(st.Orgs, st.OrgMembers, st.Invites) })
	orch.orgPlans = build("leaf/orgplan", func() *orgplan.Leaf { return orgplan.New(st.OrgPlans) })
	orch.serverPlans = build("leaf/orgplan server", func() *orgplan.Server { return orgplan.NewServer(st.ServerPlans) })
	orch.stacks = build("leaf/stack", func() *stack.Leaf { return stack.New(st.Stacks) })
	orch.envs = build("leaf/environment", func() *environment.Leaf {
		return environment.New(st.Environments, d).WithSubnets(nodes).WithPlacement(orch.envPins).WithController(orch.controllerID)
	})
	orch.tiers = build("leaf/tier", func() *tier.Leaf { return tier.New(st.Tiers) })
	orch.tiles = build("leaf/tile", func() *tile.Leaf { return tile.New(st.Tiles, d, o.vip) })
	orch.images = build("leaf/image", func() *image.Leaf { return image.New(st.Images, d) })
	orch.params = build("leaf/params", func() *params.Leaf { return params.New(st.Params) })
	orch.volumes = build("leaf/volume", func() *volume.Leaf { return volume.New(st.Volumes, d).WithShares(st.Shares).WithNode(orch.scopeNode) })
	orch.domains = build("leaf/domain", func() *domain.Leaf {
		return domain.New(st.Domains, d, ProxyContainer).WithSubnets(nodes).WithController(orch.controllerID)
	})
	orch.domainres = build("leaf/domainres", func() *domainres.Leaf { return domainres.New(st.DomainResources) })
	orch.routes = build("leaf/route", func() *route.Leaf { return route.New(st.Routes) })
	orch.hostgrant = build("leaf/hostgrant", func() *hostgrant.Leaf { return hostgrant.New(st.HostGrants) })
	orch.earlyIPs = map[string]earlyEnt{}
	orch.tiles.Early = orch.earlyLan
	orch.tiles.Node = orch.envNode
	orch.tiles.Pin = orch.pinVIP
	orch.tiles.Ship = orch.shipRun
	orch.creds = build("leaf/credential", func() *credential.Leaf { return credential.New(st.Credentials) })
	orch.regs = build("leaf/registry", func() *registry.Leaf { return registry.New(st.RegistryImages, installspec.RegistrySocket(cfg.DataDir)) })
	orch.kinds, orch.dnsOpen = append(all.Kinds(), o.kinds...), o.dnsOpen
	orch.conns = build("leaf/connector", func() *connector.Leaf { return connector.New(st.Connectors, orch.kinds) })
	orch.dns = build("leaf/dnscap", func() *dnscap.Leaf { return dnscap.New(st.DNSDomains, st.DNSRecords) })
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
			Stacks:    orch.stacks,
			S3: func(endpoint, access, secret string) mflow.S3Admin {
				return s3.Admin{Endpoint: endpoint, AccessKey: access, SecretKey: secret}
			},
			PublicBase: orch.publicBase,
		}
	})
	orch.sync = build("leaf/domain.Syncer", func() *domain.Syncer {
		return &domain.Syncer{BuildAll: orch.proxyConfigs, PushNode: orch.pushNodeProxy(proxyPush(orch.controllerID, o.push, orch.caddyFor, func(ctx context.Context, id string) error { return orch.nodeDown(ctx, id) })), Soft: orch.isWorker, After: orch.dnsKick.Kick}
	})
	orch.deploy = build("flow/deploy", func() *deploy.Flow {
		return &deploy.Flow{
			Preflight:  orch.preflight,
			Remote:     func(ctx context.Context, e store.Environment) bool { return orch.envRemote(ctx, e.ID) },
			LinkHost:   orch.linkHost,
			Tiles:      orch.tiles,
			Envs:       orch.envs,
			Stacks:     orch.stacks,
			Orgs:       orch.orgs,
			Volumes:    orch.volumes,
			Images:     orch.images,
			Releases:   orch.releases,
			Params:     orch.params,
			Tiers:      orch.tiers,
			Managed:    orch.managed,
			Domains:    orch.domains,
			Import:     orch.importForDeploy,
			Build: func(ctx context.Context, t store.Tile, commit, ref string, log io.Writer) (string, error) {
				st, err := orch.stacks.Get(ctx, t.StackID)
				if err != nil {
					return "", err
				}
				return orch.buildInto(ctx, st, t, commit, ref, log)
			},
			Cred:       orch.pullCred,
			Settings:   orch.settings,
			Jobs:       orch.jobRows,
			HostGrants: orch.hostgrant,
			VIPLock:    orch.vipLock,
			DataDir:    cfg.DataDir,
			Sync:       orch.sync.Sync,
			Engines:    orch.engines,
			Runs:       orch.runs,
			RunFirst:   orch.runFirst,
		}
	})
	orch.promote = build("flow/promote", func() *promote.Flow {
		b := orch.buildTile
		if o.build != nil {
			b = o.build
		}
		return &promote.Flow{
			D:           orch.deploy,
			Resources:   orch.domainres,
			Config:      orch.stackFile,
			Build:       b,
			DNS01:       orch.dns01,
			RouteHeld:   orch.routes.Covers,
			WriteParams: orch.writeParams,
			Placements:  orch.stackPlacements,
		}
	})
	orch.backup = build("flow/backup", func() *fbackup.Flow {
		scratch := filepath.Join(cfg.DataDir, "backups", "scratch")
		// A crash leaves unencrypted db and cert tars here; nothing runs yet.
		if err := os.RemoveAll(scratch); err != nil {
			slog.Warn("backup scratch not cleared", "dir", scratch, "error", err)
		}
		return &fbackup.Flow{
			Backups: orch.backups,
			Volumes: orch.volumes,
			Tiles:   orch.tiles,
			Scratch: scratch,
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
			Cred:     orch.pullCred,
			Settings: orch.settings,
		}
	})
	orch.upgrade = build("flow/upgrade", func() *upgrade.Flow {
		return &upgrade.Flow{
			Panel:   panel.New(d),
			Version: cfg.Version,
			Archive: orch.upgradeArchive,
			Spec:    orch.panelSpec,

			Docker:    d,
			ProxySpec: cfg.ProxySpec,
			DataDir:   registryHostDir(cfg),
		}
	})
	orch.run = build("flow/run", func() *frun.Flow {
		return &frun.Flow{
			Tiles:  orch.tiles,
			Envs:   orch.envs,
			Runs:   orch.runs,
			Jobs:   orch.jobRows,
			Deploy: orch.deploy,
			Points: orch.writeRunPoints,
		}
	})
	orch.sample = build("flow/traffic", func() *ftraffic.Flow {
		return &ftraffic.Flow{
			Tiles:   orch.tiles,
			Envs:    orch.envs,
			Stacks:  orch.stacks,
			Domains: orch.domains,
			Managed: orch.managed,
			Traffic: orch.traffic,
			Path:    cfg.Conntrack,
			Remote:  orch.workerTraffic,
		}
	})
	orch.graph = build("flow/graph", func() *graph.Flow {
		return &graph.Flow{
			Orgs:     orch.orgs,
			Stacks:   orch.stacks,
			Envs:     orch.envs,
			Tiles:    orch.tiles,
			Params:   orch.params,
			Tiers:    orch.tiers,
			Volumes:  orch.volumes,
			Domains:  orch.domains,
			Managed:  orch.managed,
			Conns:    orch.conns,
			Releases: orch.releases,
			Jobs:     orch.jobRows,
			Runs:     orch.runs,
			Canvas:   orch.canvas,
			Images:   orch.images,
			Target:   orch.deploy.Target,
			Slice:    orch.engines.SliceWord,
		}
	})
	orch.jobs = build("flow/jobs", func() *jobs.Runner {
		// ponytail: a parked job is requeued every poll and its handler
		// re-checks (DECIDE 17 (b)); a host access park resumes once its
		// stack's grant covers the ask, however the grant got there.
		return jobs.New(orch.jobRows, orch.handlers(), cfg.DataDir, jobs.Options{
			ParamSet: func(ctx context.Context, j store.Job) (bool, error) {
				ask, approval := hostgrant.Parse(*j.WaitingParam)
				if !approval {
					return true, nil
				}
				var p struct {
					HostAccess *hostAccess `json:"host_access"`
				}
				if json.Unmarshal([]byte(j.Payload), &p) != nil || p.HostAccess == nil {
					return false, nil
				}
				have, err := orch.hostgrant.Of(ctx, p.HostAccess.Stack)
				return err == nil && len(ask.Missing(have)) == 0, err
			},
			Workers: func(ctx context.Context) (int, error) { return orch.settings.Int(ctx, "workers") },
			// A run holds its own clock: the tile's timeout_minutes.
			Uncapped: map[jobs.Kind]bool{kindRun: true, kindMoveEnv: true},
		})
	})
	orch.sched = build("flow/schedule", func() *schedule.Runner {
		return schedule.New(schedule.Drivers{
			Schedules: orch.backups.AllSchedules,
			Backup:    orch.scheduleBackup,
			Orphans: func(ctx context.Context) error {
				_, err := orch.enqueue(ctx, kindOrphans, nil, "orphans")
				return err
			},
			PanelBackup: func(ctx context.Context) error {
				_, err := orch.enqueue(ctx, kindPanelBackup, panelBackupJob{Scheduled: true}, orch.panelLock(true)...)
				return err
			},
			Cleanup: func(ctx context.Context) error {
				_, err := orch.enqueue(ctx, kindCleanup, nil, "cleanup")
				return err
			},
			RegistryClean: func(ctx context.Context) error {
				_, err := orch.enqueue(ctx, kindRegistryClean, registryCleanJob{}, "registry-clean")
				return err
			},
			Settings: orch.scheduleSettings,
			Watch:    orch.watchTick,
			Crons:    orch.deployedCrons,
			Cron:     orch.scheduleCron,
			Traffic:  orch.trafficTick,
			Metrics:  orch.metricsTick,
			Logs:     orch.logshipTick,
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
	if err := orch.conns.Reshape(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connector settings: %w", err)
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
	// Before the workers start, so no deploy can declare a chain first.
	if err := orch.rebuildVIPs(ctx); err != nil {
		slog.Warn("vip: boot rebuild failed", "err", err)
	}
	orch.vipLegacy(ctx)
	orch.settleRestored(ctx)
	if err := orch.backups.Interrupted(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("close interrupted backups: %w", err)
	}
	if err := orch.jobs.Start(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("start job runner: %w", err)
	}
	if err := orch.resumeMoves(ctx); err != nil {
		slog.Warn("moves: resume failed", "err", err)
	}
	orch.redeployRestored(ctx)
	// Tracked like the rollover steps: Close waits for it, stopNodes cancels it.
	orch.startAlerts()
	orch.startDNSSocket()
	if created {
		orch.defaultControllerAddress(ctx) // a panel address from the boot config
	}
	orch.detach(context.Background(), orch.dailyChores)
	if err := orch.sched.Boot(ctx); err != nil {
		// A bad schedule row is reported, never fatal: the rest still run.
		slog.Warn("scheduler: entries skipped", "err", err)
	}
	if w := ftraffic.Check(cfg.Conntrack); w != "" {
		slog.Warn(w, "path", cfg.Conntrack)
	}
	// Detached: Close waits for it, so it never queries a closed database
	// or holds a connection that outlives Close.
	orch.detach(context.Background(), func(ctx context.Context) {
		if err := orch.domains.ReopenIngress(ctx); err != nil {
			slog.Warn("proxy: ingress rejoin failed", "err", err)
		}
		if err := orch.sync.Sync(ctx); err != nil {
			slog.Warn("proxy: boot push failed", "err", err)
		}
	})
	if orch.runMesh != nil {
		orch.runMesh()
	}
	// A real host only (MeshWG set): a fake host or a test has no registry
	// to fill, and in a test this ran beside the test's own setup.
	if orch.localWorker != nil {
		orch.detach(context.Background(), func(context.Context) {
			// An upgrade ran the old panel's EnsureRegistry: bring the
			// registry to this build's spec before anything uses it.
			cctx, err := orch.withController(orch.meshCtx)
			if err == nil {
				err = orch.upgrade.EnsureRegistry(cctx)
			}
			if err != nil {
				slog.Warn("registry: not ensured", "err", err)
			}
			// Before the publish, which starts the worker rollout: a Docker
			// restart for the cap must not land mid-rollout.
			orch.controllerLogCap(orch.meshCtx)
			orch.publishSystemImages(orch.meshCtx)
			orch.clearUpgrader(orch.meshCtx) // the Update that started this panel left it
		})
		orch.startSyncObserve() // brings it to this build's version
	} else {
		orch.markStackrPublished() // no registry to fill
		if k, err := orch.observeKnobs(ctx); err == nil {
			orch.observe.on.Store(k.on)
		}
	}
	return orch, nil
}

// openDB connects and brings the schema up to date.
func openDB(ctx context.Context, path string) (*sqlx.DB, error) {
	db, err := sqlite.ConnectContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := sqlite.Migrate(db, appdb.MigrateConfig()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
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
	} {
		if v != "" {
			boot[k] = v
		}
	}
	return boot
}

// Close stops the scheduler and the job runner, then releases the database.
func (o *Orchestrator) Close() error {
	o.stopOnce.Do(func() {
		close(o.stopNodes)
		if o.stopMesh != nil {
			o.stopMesh()
		}
	})
	o.trustAsync.Wait()
	o.alertsDone.Wait()
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
