package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	_ "github.com/joho/godotenv/autoload"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/config"
	db "github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/FyrmForge/hamr/pkg/janitor"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/FyrmForge/hamr/pkg/websocket"
	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/config/orgconf"
	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/api"
	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/stream"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/web/components"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cigate"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/forward"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/imagewatch"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/netpool"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/nodes"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	stackruntime "github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/volmove"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcmail "github.com/FyrmForge/stackr/internal/stackrd/service/mail"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	appdb "github.com/FyrmForge/stackr/internal/stackrd/store/db"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
)

// version is set at build time via ldflags.
var version = "dev"

var (
	envPort = config.GetEnvOrDefaultInt("PORT", 8080)
	// DEV_MODE defaults to false (fail closed in prod). Local dev sets
	// DEV_MODE=true via .env so the scaffolded `.env` ships with it set
	// explicitly. This makes the STRIPE_MOCK production guard actually
	// guard, a leftover STRIPE_MOCK=true in a prod deploy without
	// DEV_MODE explicitly set would otherwise slip through.
	envDevMode        = config.GetEnvOrDefaultBool("DEV_MODE", false)
	envBaseURL        = config.GetEnvOrDefault("BASE_URL", "")
	envDatabasePath   = config.GetEnvOrDefault("DATABASE_PATH", "./data/stackr.db")
	envStaticBaseURL  = config.GetEnvOrDefault("STATIC_BASE_URL", "/static")
	envStoragePath    = config.GetEnvOrDefault("STORAGE_PATH", "./uploads")
	envTrustedProxies = config.GetEnvCSV("TRUSTED_PROXIES")
	envDataDir        = config.GetEnvOrDefault("DATA_DIR", "./data")
	// STACKR_TLS=off serves everything on plain HTTP: LAN and throwaway hosts
	// no CA can reach. Anything else keeps ACME on. install.sh sets it off for
	// localhost, a bare IP, or any privately-reachable name.
	envTLSOff    = strings.EqualFold(config.GetEnvOrDefault("STACKR_TLS", "on"), "off")
	registryPort = config.GetEnvOrDefault("REGISTRY_PORT", "5000")
)

// cookieSecureFor decides whether session, flash and CSRF cookies are marked
// Secure. A Secure cookie is never sent back over plain HTTP, so this has to
// follow whether the install serves TLS. Keying it off DEV_MODE alone locked
// every STACKR_TLS=off install out of its own panel: correct credentials, no
// cookie stored, straight back to /login. DEV_MODE still forces it off, for
// local http dev.
func cookieSecureFor(devMode, tlsOff bool) bool { return !devMode && !tlsOff }

func main() {
	// `stackrd agent` is the node agent (cmd/stackrd/agent.go): same binary,
	// same image, different entrypoint. Checked before flag parsing because
	// it shares none of the panel's flags or its config.
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		runAgent()
		return
	}

	generateFlag := flag.Bool("generate", false, "generate static pages and exit")
	dumpOpenAPI := flag.Bool("dump-openapi", false, "write the OpenAPI spec to docs/openapi.json and exit")
	flag.Parse()

	// Spec dump, registration only reflects Go types (handlers never run),
	// so nil deps are fine and no config or DB is needed.
	if *dumpOpenAPI {
		if err := writeOpenAPISpec("docs/openapi.json"); err != nil {
			fmt.Fprintln(os.Stderr, "openapi:", err)
			os.Exit(1)
		}
		return
	}

	log := logging.New(!envDevMode)
	slog.SetDefault(log)

	components.StaticBaseURL = envStaticBaseURL

	// Base URL (CORS and the panel route). The session cookie is host-only on
	// purpose: a Domain attribute would hand it to every app under the panel.
	baseOrigin, baseDomain, err := config.ParseBaseURL(envBaseURL)
	if err != nil {
		log.Error("invalid BASE_URL", "error", err)
		os.Exit(1)
	}
	components.BaseURL = baseOrigin

	// Server.
	srv, err := server.New(
		server.WithPort(envPort),
		server.WithDevMode(envDevMode),
		server.WithStaticDir("frontend/static"),
		server.WithStaticDistDir("frontend/dist"),
		server.WithGeneratedDir("generated"),
		// TRUSTED_PROXIES: comma-separated CIDRs of upstream proxies/load
		// balancers allowed to set X-Forwarded-For (drives client-IP detection
		// and the rate-limit key). Empty/unset ignores X-Forwarded-For so a
		// direct client can't spoof its IP; set it to your LB ranges behind one.
		server.WithTrustedProxies(envTrustedProxies...),
		// hamr defaults to 2MB, which is a sane API limit and a useless one for
		// a panel whose file browser writes into docker volumes. Not unbounded
		// either: multipart parsing buffers 32MB per request in memory and
		// spills the rest to os.TempDir(), so a generous limit is a way to fill
		// a small /tmp. Raise MAX_UPLOAD_SIZE if the host has room for it.
		//
		// The limit is enforced at the root, outside the site group, so
		// exceeding it still returns a raw JSON 413 that htmx swaps in place of
		// the file browser, one more reason to keep it clear of real uploads.
		server.WithMaxBodySize(config.GetEnvOrDefault("MAX_UPLOAD_SIZE", "256M")),
		// Gzip must not wrap WebSocket upgrades: the compressed writer never
		// flushes the 101 and the handshake hangs (only bites in prod, dev
		// mode disables gzip, which is why local dev never saw it).
		server.WithGzipConfig(server.GzipConfig{
			Enabled: true,
			Skipper: func(c echo.Context) bool {
				return strings.EqualFold(c.Request().Header.Get("Upgrade"), "websocket")
			},
		}),
	)
	if err != nil {
		log.Error("failed to create server", "error", err)
		os.Exit(1)
	}

	if baseOrigin != "" {
		srv.Echo().Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins:     []string{baseOrigin},
			AllowCredentials: true,
		}))
	}

	// Static page generation, no heavy deps needed.
	web.RegisterStaticPages(srv)
	if *generateFlag {
		if err := srv.GenerateStatic("generated"); err != nil {
			log.Error("generate static pages failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// Database.
	connectCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	database, err := db.ConnectContext(connectCtx, envDatabasePath)
	cancel()
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}

	// Run migrations at startup. appdb.Migrate rather than the hamr helper: a
	// database predating the migration squash needs its version reconciled
	// before the migrator will look at it.
	if err := appdb.Migrate(database); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
	log.Info("migrations completed")
	store := sqlite.NewStore(database)

	// Secrets-at-rest: load (or create) the master key. Must precede anything
	// that reads or writes tiles, their env is encrypted at rest.
	if err := secrets.Load(filepath.Join(envDataDir, "keys", "master.key")); err != nil {
		log.Error("loading master key failed", "error", err)
		os.Exit(1)
	}

	cookieSecure := cookieSecureFor(envDevMode, envTLSOff)

	// Sessions.
	sessionManager := auth.NewSessionManager(store,
		auth.WithCookieSecure(cookieSecure),
	)

	// Auth service.
	authService := service.NewAuthService(store)

	// File storage (local).
	fileStorage, err := storage.NewLocalStorage(envStoragePath)
	if err != nil {
		log.Error("failed to init storage", "error", err)
		os.Exit(1)
	}

	// Docker runtime + deploy engine.
	rt, err := stackruntime.New()
	if err != nil {
		log.Error("docker runtime init failed", "error", err)
		os.Exit(1)
	}
	// An attachable overlay, not a bridge: traefik and the registry are swarm
	// services and a service cannot join a bridge at all, while the panel and
	// the forward relays are plain containers and need the attachable half.
	if err := rt.EnsureOverlay(context.Background(), stackruntime.NetworkName); err != nil {
		log.Error("ensure docker network failed", "error", err)
		os.Exit(1)
	}
	// rows is the handle the layer below writes its rows through: the
	// environment's overlay, a shared instance's overlay, where traefik
	// answered. Everything under infra/ needs it, and almost everything under
	// infra/ is built before the services that fill it in — the deploy
	// engine, the managed-tile service and the job runner all come first, and
	// the services are built on top of them.
	//
	// So it is one pointer, handed out empty and filled once further down.
	// The alternative is reordering a wiring block whose order is load-
	// bearing, to remove an indirection that costs nothing.
	//
	// Everything that dereferences it does so on a request, long after the
	// assignment below. The one exception is Backfill, which runs at boot, so
	// its two fields are set before it.
	rows := &service.Rows{}

	// Tenant networks come from a pre-created pool of overlays, because a
	// swarm service cannot join a network without rolling its tasks
	// (docs/plans/30-docker-swarm.md). Create them before anything claims one.
	if err := netpool.Ensure(context.Background(), rt); err != nil {
		log.Error("overlay pool failed", "error", err)
		os.Exit(1)
	}
	// A crash between deleting an env and returning its network leaves a free
	// name with someone else's containers still on it.
	netpool.Sweep(context.Background(), store, rt)
	// Backfill used to run here. It writes the environment row, so it now
	// needs the environment service, which is built much further down — the
	// call moved with it rather than the wiring moving up.
	// One signer for the whole process. The registry service and the web token
	// route both need it, and both used to load it themselves: on a fresh data
	// dir that raced, and the endpoint could end up signing with a key the
	// registry's mounted cert did not match (docs/plans/39-codex-review-fixes.md
	// point 4). A load failure is not fatal here; the token route answers 503
	// and EnsureManaged refuses, rather than taking the panel down.
	registrySigner, err := registry.LoadSigner(filepath.Join(envDataDir, "registry"))
	if err != nil {
		log.Error("registry token keypair", "error", err)
		registrySigner = nil
	}
	// The registry is not optional any more: every build pushes to it, so a
	// deploy has nowhere to get its image from if it is not up
	// (docs/plans/30-docker-swarm.md, decision 3). Pulling registry:2 can be
	// slow, so it runs alongside boot rather than blocking it; the first
	// deploy fails with the push error if it is not ready yet.
	// One owner for the registry row and for the credentials the panel issues
	// to itself. Built here rather than with the other services further down
	// because the registry comes up during boot and several things below take
	// it; it needs nothing but the store.

	// One owner for the registry row and for the credentials the panel issues
	// to itself. Built here rather than with the other services further down
	// because the registry comes up during boot and several things below take
	// it; it needs nothing but the store.
	registrySvc := service.NewRegistryService(store)
	go func() {
		if _, err := registry.EnsureManaged(context.Background(), store, registrySvc, rt, registrySigner, envDataDir, registryPort, baseOrigin); err != nil {
			log.Error("managed registry start failed", "error", err)
		}
	}()
	// A restart dropped every forward websocket, so any relay still around is
	// a leftover; also undo the self-attachments the pre-relay design made.
	rt.SweepForwardState(context.Background())

	// One owner for "may this principal do this verb in this org". The level
	// per verb lives in its table; the surfaces ask, they no longer decide.
	accessSvc := service.NewAccessService(store)

	// WebSocket hub. Clients join rooms ({"action":"join","room":"..."}) and
	// get data-free "changed" events pushed by the notifier (live UI updates).
	//
	// The subject comes off the session cookie on the upgrade, and a join is
	// checked against it. Neither used to happen: /ws is registered outside
	// the site group, so the connection carried no identity at all, and the
	// hub joined whatever room the client named. The payloads are event kinds
	// with no data, so what leaked was activity timing — when any stack
	// deploys, when anything on the box changes — to anyone who could reach
	// the port.
	var hub *websocket.Hub
	hub = websocket.NewHub(
		websocket.WithLogger(log),
		websocket.WithSubjectIDFunc(func(r *http.Request) string {
			ck, err := r.Cookie(sessionManager.CookieName())
			if err != nil || ck.Value == "" {
				return ""
			}
			// Not r.Context(): this runs after the connection has been
			// hijacked for the upgrade, and a cancelled context here would
			// refuse every join on the box.
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sess, err := sessionManager.ValidateSession(sctx, ck.Value)
			if err != nil || sess == nil {
				return ""
			}
			return sess.SubjectID
		}),
		websocket.WithOnMessage(func(c *websocket.Client, msg []byte) {
			var m struct {
				Action string `json:"action"`
				Room   string `json:"room"`
			}
			if json.Unmarshal(msg, &m) != nil {
				return
			}
			switch m.Action {
			case "join":
				if !roomReadable(context.Background(), store, accessSvc, c.SubjectID, m.Room) {
					return
				}
				hub.JoinRoom(c, m.Room)
			case "leave":
				hub.LeaveRoom(c, m.Room)
			}
		}))
	notifier := notify.New(hub, store)

	// Nothing else can finish a deploy that died with the previous process.
	if err := store.SweepStaleRuns(context.Background()); err != nil {
		log.Error("sweeping stale runs failed", "error", err)
	}

	streamHub := stream.NewHub()
	// Multi-node. On a single node none of this does anything visible: the
	// node dispatcher answers "here" to every call and keeps using the
	// socket, and the servers list has one row
	// (docs/plans/31-node-agent-open-questions.md, which calls move).
	nodeDispatch := agent.New(rt, version, envDataDir)
	// The one door for every docker call. Built before anything that
	// touches a container, so nothing can be constructed without it
	// (docs/plans/35-cluster.md).
	clus := cluster.New(rt, nodeDispatch, store)
	engine := deploy.NewEngine(store, rt, clus, streamHub, envDataDir, notifier, rows, registrySvc)

	// GitHub connectors: private-repo clone auth, ghcr pulls, PR feedback.
	gh := githubapp.New(store, service.NewConnectorService(store), baseOrigin)
	engine.GitAuth = gh.CloneAuth
	engine.RegistryAuth = gh.RegistryAuth
	engine.OnFinish = func(tile *repo.Tile, d *repo.Deployment) {
		go gh.DeployFinished(context.Background(), tile, d)
	}

	// Traefik proxy (started in background; pulls the image on first boot).
	px := proxy.New(clus, store, envDataDir,
		config.GetEnvOrDefault("TRAEFIK_HTTP_PORT", "80"),
		config.GetEnvOrDefault("TRAEFIK_HTTPS_PORT", "443"),
		config.GetEnvOrDefault("ACME_EMAIL", ""),
		// How Traefik reaches the panel from inside the docker network, for
		// the panel's own route. The default matches the alias the deploy
		// script gives the stackr container.
		config.GetEnvOrDefault("PANEL_INTERNAL_URL", fmt.Sprintf("http://stkr-panel:%d", envPort)),
		// The panel's own public hostname, so Traefik can route to it without
		// anybody configuring a domain first.
		baseDomain,
		envTLSOff,
	)
	// Everything above the service line writes traefik config through this,
	// never through *proxy.Proxy: one managed gate, one error policy, and one
	// serialized rewrite-and-restart.
	pxSvc := svcproxy.New(store, px, registrySvc, rt, registrySigner, envDataDir, registryPort, baseOrigin)
	// Immediately, and before anything renders: the proxy reads every one of
	// its settings rows back through this. It is a setter rather than a
	// constructor argument only because pxSvc is built on px.
	px.UseSettings(pxSvc)
	px.UseRegistries(registrySvc)
	// One owner for the defaults cascade at all four levels: the config-file
	// gate applies at org, stack and env, and every write resyncs the proxy.
	// Built here rather than with the rest of the services because boot reads
	// settings before most of them exist — the install seed and the nightly
	// cleanup switch both go through it.
	settingsSvc := service.NewSettingsService(store, pxSvc)
	// The PR comment reads its stored plan preview back through the same
	// owner the webhook handler wrote it with.
	gh.UseSettings(settingsSvc)
	gh.UseDeploys(rows)
	// Before Traefik starts: it reads the trusted proxy settings.
	if err := seedInstall(context.Background(), store, settingsSvc,
		config.GetEnvOrDefault("ROOT_DOMAIN", ""),
		config.GetEnvOrDefault("TRUST_CLOUDFLARE", ""),
		config.GetEnvOrDefault("TRUSTED_PROXY_CIDRS", ""),
		px.RefreshCloudflare); err != nil {
		log.Error("seeding install answers failed", "error", err)
	}
	go func() {
		// Boot is the one caller that takes the error rather than a log line
		// from inside the service: nothing is serving yet.
		if err := pxSvc.EnsureTraefikNow(context.Background()); err != nil {
			log.Error("traefik startup failed", "error", err)
		}
		// Dynamic configs are renders of DB state, regenerate them so a
		// wiped/restored data dir heals itself.
		pxSvc.Resync(context.Background())
	}()

	// Metrics sampler + nightly cleanup.
	// The slice reader is wired here rather than imported inside the sampler:
	// metrics sits below managedtiles, and that edge is the one that closed
	// the cycle keeping managedtiles off the node-aware runtime.
	sampler := metrics.NewSampler(store, rows, clus, notifier).WithSliceReader(
		func(ctx context.Context, inst *repo.Tile, names []string) (map[string]metrics.SliceRead, error) {
			read, err := managedtiles.NewService(clus, store, rows).SliceStats(ctx, inst, names)
			if err != nil || read == nil {
				return nil, err
			}
			out := make(map[string]metrics.SliceRead, len(read))
			for name, st := range read {
				out[name] = metrics.SliceRead{Xacts: st.Xacts, Size: st.Size}
			}
			return out, nil
		})
	go sampler.Run(context.Background())
	go sampler.RunFlows(context.Background())
	go func() {
		accessLog := filepath.Join(envDataDir, "traefik", "access.log")
		for {
			time.Sleep(24 * time.Hour)
			// hard truncate over 50MB, rotation comes with the
			// proper logs viewer.
			if fi, err := os.Stat(accessLog); err == nil && fi.Size() > 50<<20 {
				_ = os.Truncate(accessLog, 0)
			}
			if v, _ := settingsSvc.Value(context.Background(), settings.KeyCleanupEnabled); v == "1" {
				if out, err := clus.Prune(context.Background(), clus.Self(context.Background())); err != nil {
					log.Error("docker cleanup failed", "error", err)
				} else {
					log.Info("docker cleanup done", "output", out)
				}
				// Deleting a tag only unlinks its manifest; the blobs stay
				// until this sweep, which takes a read lock on the whole
				// registry store and so is not something to run per delete.
				if out, err := registry.GarbageCollect(context.Background(), rt); err != nil {
					log.Error("registry garbage collection failed", "error", err)
				} else {
					log.Info("registry garbage collection done", "output", out)
				}
			}
		}
	}()

	dbService := managedtiles.NewService(clus, store, rows)

	// Scheduled jobs (cron commands for apps).
	jobsService := jobs.NewService(store, clus, notifier, rows, nil, registrySvc, rows)
	// Function tiles with the on-deploy trigger fire once their own build
	// lands, chained onto the GitHub hook above rather than replacing it.
	prevFinish := engine.OnFinish
	engine.OnFinish = func(tile *repo.Tile, d *repo.Deployment) {
		if prevFinish != nil {
			prevFinish(tile, d)
		}
		if tile.Kind == "function" && tile.RunOnDeploy && d.Status == "done" {
			if _, err := jobsService.StartApp(context.Background(), tile.ID, jobs.TriggerDeploy, ""); err != nil {
				log.Error("on-deploy run not queued", "tile", tile.ID, "error", err)
			}
		}
		// Baseline the pulled digest so the image watcher compares against
		// what actually runs (git builds carry no registry digest).
		if d.Status == "done" && tile.SourceType == "image" {
			if dg, err := clus.LocalDigest(context.Background(), tile.ImageRef); err == nil && dg != "" {
				_ = store.SetTileImageDigest(context.Background(), tile.ID, dg)
			}
		}
	}

	// Backups: dumps, volume archives, and the panel's own database. Holds the
	// live *sqlx.DB because VACUUM INTO is the only consistent way to copy the
	// file while stackr is serving.
	backupService := backup.NewService(store, nil, dbService, clus, database, envDataDir, version)

	// One owner for "re-register the schedules". Everything that cascades a
	// cron_jobs or backups row calls this instead of LoadSchedules directly.
	sched := scheduler.New(jobsService, backupService)
	if cronErr, bkErr := sched.Boot(context.Background()); cronErr != nil || bkErr != nil {
		log.Error("loading schedules failed", "cron", cronErr, "backups", bkErr)
	}

	// One owner for "put this tile into that runtime state". Both routers get
	// the same value, so a stop over the API and a stop in the panel perform
	// the same side effects — the nudge to open canvases included, which the
	// API used to skip.
	lifecycle := service.NewTileLifecycleService(store, clus, jobsService, dbService, sched, notifier)
	// The knot: the job runner writes cron_runs through the service, and the
	// service runs jobs through the runner. The runner is built first, so the
	// row owner is handed over here. Nothing reads it until a job fires.
	jobsService.WithRuns(lifecycle)

	// One owner for the tile row and for everything a write to it has to
	// cascade. gate answers "may this stack be written to, and does the write
	// queue for review" for every surface with one rule set.
	gate := service.NewGateService(store)
	tiles := service.NewTileService(store, clus, pxSvc, dbService, sched, engine, gate)
	telemetry := service.NewTileTelemetryService(store, clus)
	domains := service.NewDomainService(store, pxSvc, gate)
	resources := service.NewDomainResourceService(store, pxSvc)

	// One owner for a managed database instance and for the slices cut out of
	// it. Delete used to be four teardowns and none of them was complete.
	instances := service.NewManagedInstanceService(store, dbService, tiles, gate, notifier)
	slices := service.NewSliceService(store, dbService, engine, notifier)

	// Handed to everything that touches a container or a volume: both live on
	// one node's disk, and the manager's own socket answers only for itself.
	// Set here rather than in the constructors because the dispatcher needs
	// the runtime, which these were built from.
	// A swarm with more than one node runs Ensure on every boot, not only
	// when the service is missing. It is the upgrade path: the new panel
	// points the agent service at its own image (a release tag, or a digest in
	// the managed registry), so the agents roll to the same build
	// (docs/plans/30-docker-swarm.md, step 7; 44-agent-image-published.md).
	// Skipping it while the service exists leaves every node on the old agent,
	// which the panel then refuses on the version header.
	// One owner for a node's life after it joins, and for the one agent-ensure
	// retry policy the boot path used to have to itself.
	nodeLifecycle := service.NewNodeService(store, rt, registrySvc, envDataDir)

	go func() {
		ctx := context.Background()
		ns, err := rt.ListNodes(ctx)
		if err != nil || len(ns) < 2 {
			return
		}
		// The retries are NodeService's, the same ones the panel's Add node
		// gets: on the registry path the push authenticates against this
		// panel, which is not listening yet on the first try.
		_ = nodeLifecycle.EnsureAgent(ctx)
	}()
	// Upgrades from the panel. The release check runs at boot and daily so the
	// rail badge has something to show; the update page also checks on open.
	adminService := service.NewAdminService(store, rt, clus, backupService, envDataDir, version)
	go func() {
		for {
			if _, err := adminService.CheckUpgrade(context.Background()); err != nil {
				log.Warn("upgrade check", "error", err)
			}
			time.Sleep(24 * time.Hour)
		}
	}()
	nodeSvc := &nodes.Service{Store: store, RT: rt,
		Rows: rows}
	// Sync writes the manager's own swarm id onto its servers row. Nothing
	// else does, and every screen keyed on servers.node_id (tile placement,
	// /servers/local, volume delete) reads the manager as not joined until
	// someone happens to open /servers.
	if _, err := nodeSvc.Sync(context.Background()); err != nil {
		log.Error("node sync", "error", err)
	}
	mover := volmove.New(store, rows, clus, engine, dbService)
	// A move receiver left behind by a panel that died mid-move holds a
	// volume open and squats the alias the next move wants.
	rt.SweepMoveReceivers(context.Background())
	// The manager times a TCP connect to every other node's swarm port. No
	// ICMP, no agent, no extra port: 7946 already has to be open or swarm
	// does not work at all (docs/plans/32-multi-node-ui.md, servers list).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			_, _ = nodeSvc.Sync(context.Background())
			nodeSvc.Ping(context.Background())
		}
	}()

	// Live port-forward sessions, shared by the API (writes) and the canvases
	// (read who is connected).
	forwards := forward.New()

	// Config-as-code. Built here rather than inside either router because the
	// web canvas and the API drive the same plan → approve → apply flow, and two
	// copies would be two things to keep in step.
	//
	// Ops is a variable rather than an inline literal because of a knot: the
	// environment service tears down through Ops, and Ops now writes its rows
	// back through the environment service. Two of Ops' fields therefore can
	// only be filled in after the service exists, and the service is handed
	// &ops so it sees them when they are. applier.Ops is assigned from the
	// finished value further down — a copy taken here would be a copy with a
	// nil Envs, and the nil would not show up until a teardown.
	ops := envops.Ops{Store: store, RT: rt, Cluster: clus, PX: pxSvc, DBs: dbService,
		Tiles: tiles, Sched: sched, Domains: domains, Resources: resources}
	applier := stackconf.Applier{
		Planner:   stackconf.Planner{Store: store, Src: gh},
		DBs:       dbService,
		Instances: instances,
		Slices:    slices,
		Engine:    engine,
		Jobs:      jobsService,
		Sched:     sched,
	}
	// One owner for which tiles may be deployed. The engine below the line
	// still writes the deployment row; what moved up is the rules, which had
	// drifted between the two surfaces and had a fourth private copy inside
	// the variable service.
	deploySvc := service.NewDeployService(store, engine)

	// One owner for every write to the variables table. The replanner is the
	// config planner, which detaches its own walk: a variable write must not
	// wait on one, and the API's request budget could not have afforded it.
	vars := service.NewVariableService(store, deploySvc, tiles, applier.Planner, notifier)

	// Registry watcher (per-tile update_policy) on a 1-minute janitor tick;
	// the real cadence is the image_check_interval setting, due-checked
	// in-task because janitor schedules are fixed at AddTask time.
	//
	// Built here rather than with the other background tasks above because it
	// is a service now, not a janitor task with a store: an auto update goes
	// through DeployService and ManagedInstanceService, so it has to come
	// after both.
	imageWatch := &service.ImageWatchService{Store: store, Notifier: notifier,
		Cluster: clus, GH: gh, Client: &imagewatch.Client{},
		Deploys: deploySvc, Instances: instances}
	jan := janitor.New(janitor.WithTimeout(2*time.Minute), janitor.WithLogger(log))
	jan.AddTask("@every 1m", imageWatch)
	// CI gate: releases (or fails) push deploys parked behind wait_for_ci.
	jan.AddTask("@every 1m", &cigate.Gate{Store: store, Deploys: rows, Engine: engine, GH: gh, Notifier: notifier})
	if err := jan.Start(context.Background()); err != nil {
		log.Error("janitor start failed", "error", err)
	}

	// One owner for the environment row. envops does the teardown below the
	// line; the rules, the gate and the two tables nothing used to clean up
	// are here.
	envSvc := service.NewEnvironmentService(store, &ops, sched, applier.Planner, gate)

	// The other half of the knot described at ops above: fill the two fields
	// in, then hand the applier the finished value.
	ops.Envs, ops.Vars = envSvc, vars
	applier.Ops = ops

	// And the other half of rows, declared near the top: everything under
	// infra/ has been holding this pointer since before either service
	// existed.
	rows.Envs, rows.Tiles, rows.Deploys = envSvc, tiles, deploySvc
	rows.Slices, rows.Instances, rows.Vars = slices, instances, vars
	rows.Telemetry, rows.Nodes = telemetry, service.NewNodeService(store, rt, registrySvc, envDataDir)
	// The proxy is built near the top and records where traefik landed on
	// each environment's overlay, which is a write to the environment row.
	px.UseEnvs(envSvc)

	// An environment whose network column is empty has no overlay, and only a
	// deploy ever fills it — so a freshly created environment cannot be port
	// -forwarded into until something in it is deployed, and the proxy leaves
	// it out of its network-to-env map without saying so. Give them one at
	// boot instead. Runs here rather than next to Sweep because it writes the
	// environment row, and the service that owns that row is only wired now.
	netpool.Backfill(context.Background(), store, envSvc, rt)

	// One owner for the bucket a backup is written to, and one for the
	// schedules pointed at it. Three creators had three rule sets; the
	// destination's visibility rule was written out three times.
	// One owner for a share or pool: the server id comes from the caller,
	// the sub-path name is slugified before it is checked, and a delete knows
	// the difference between a server's pool and an org's share.
	storageSvc := service.NewStorageService(store, clus)

	// One owner for the registry rows: the managed one cannot be deleted from
	// either surface now, and one in-use matcher decides whether a tag may go.

	// One owner for the pull-request environment settings: comment: and
	// status: are the file's keys, so a panel edit to them goes through the
	// gate rather than being silently reverted at the next pull request.
	prenvSvc := service.NewPREnvService(store, gate)

	destSvc := service.NewBackupDestinationService(store, sched)
	scheduleSvc := service.NewBackupScheduleService(store, destSvc, sched, gate)
	// Same knot as the job runner's, one table over.
	backupService.WithRuns(scheduleSvc)
	// Set after construction rather than in the literal: the schedule service
	// needs the gate and the scheduler, which are built alongside the applier.
	applier.Schedules = scheduleSvc

	// One owner for the stack row. The rename goes through the config
	// applier, which is the only implementation that stops the tiles under
	// the old slug and brings them back under the new one.
	// One owner for what a change of standing has to tear down: a demotion,
	// a removal and a stack move all leave working share links behind, and a
	// websocket room outlives the membership that let it in.
	revokeSvc := service.NewRevokeService(store, notifier)

	stackSvc := service.NewStackService(store, applier.Ops, sched, envSvc, applier, gate, revokeSvc)

	orgRunner := &orgconf.Runner{Store: store, Src: gh, Stacks: applier.Planner, Applier: applier, Resources: resources, StackSvc: stackSvc}

	// One durable runner for long work (docs/plans/33-workqueue.md). Applies
	// go on it first: they were the only long job with no queue at all, and
	// they ran on the HTTP request's context, so a browser that stopped
	// waiting killed the apply half way through and the failure was never
	// recorded.
	work := workqueue.New(store, service.NewWorkItemService(store))
	stackconf.RegisterApply(work, applier)
	stackconf.RegisterPromote(work, applier)
	// The org-level apply was the last one left running inline. It is the
	// slower of the two — a bucket probe per declared share and a repository
	// fetch per declared stack — and a request that died mid-apply took the
	// record of the failure with it.
	orgconf.RegisterApply(work, orgRunner)
	// The ladder, once there is a queue to put a promote on. Nothing about a
	// promote runs on the request any more.
	releaseSvc := service.NewReleaseService(store, stackconf.Queue{Q: work})
	// One owner for a config plan after it exists: replan, approve, reject.
	planSvc := service.NewPlanService(store, applier.Planner, stackconf.Queue{Q: work})
	engine.WithWork(work)
	// Every kind registers before Start: boot recovery fails a row whose kind
	// has no handler, which would skip its cleanup.
	backupService.WithWork(work)
	mover.WithWork(work)
	jobsService.WithWork(work)
	work.Start(context.Background())
	// Optional: with no provider configured this is nil and invites fall back
	// to a link the inviter passes on by hand.
	mailer := svcmail.New(mail.FromEnv(), baseOrigin)
	if mailer.Enabled() {
		log.Info("outbound mail configured")
	}

	// One owner for membership: one role whitelist, one expiry rule, one
	// last-owner guard, and the invite mailed from both surfaces.
	memberSvc := service.NewMemberService(store, mailer, revokeSvc)
	orgSvc := service.NewOrgService(store)
	auditSvc := service.NewAuditService(store)
	graphSvc := service.NewGraphService(store)
	connectorSvc := service.NewConnectorService(store)
	notificationSvc := service.NewNotificationService(store)

	api.RegisterRoutes(srv, &api.Deps{
		Store:          store,
		Cluster:        clus,
		Engine:         engine,
		Runtime:        rt,
		Proxy:          pxSvc,
		Jobs:           jobsService,
		Backups:        backupService,
		Scheduler:      sched,
		Lifecycle:      lifecycle,
		Tiles:          tiles,
		Telemetry:      telemetry,
		Domains:        domains,
		Resources:      resources,
		Instances:      instances,
		Slices:         slices,
		Variables:      vars,
		Environments:   envSvc,
		Stacks:         stackSvc,
		Gate:           gate,
		Schedules:      scheduleSvc,
		Destinations:   destSvc,
		Storage:        storageSvc,
		Settings:       settingsSvc,
		Access:         accessSvc,
		Registries:     registrySvc,
		PREnvs:         prenvSvc,
		Members:        memberSvc,
		Orgs:           orgSvc,
		Audit:          auditSvc,
		NodeService:    nodeLifecycle,
		AuthService:    authService,
		OrgConfig:      orgRunner,
		Deploys:        deploySvc,
		Releases:       releaseSvc,
		Plans:          planSvc,
		Mail:           mailer,
		Forwards:       forwards,
		Notifier:       notifier,
		Applier:        applier,
		Work:           work,
		Nodes:          nodeSvc,
		DataDir:        envDataDir,
		RegistrySigner: registrySigner,
		Version:        version,
	})

	web.RegisterRoutes(srv, &web.Deps{
		Store:          store,
		BaseURL:        baseOrigin,
		StaticBaseURL:  envStaticBaseURL,
		DevMode:        envDevMode,
		CookieSecure:   cookieSecure,
		SessionManager: sessionManager,
		AuthService:    authService,
		FileStorage:    fileStorage,
		Hub:            hub,
		Notifier:       notifier,
		StreamHub:      streamHub,
		Engine:         engine,
		Runtime:        rt,
		RegistrySigner: registrySigner,
		Proxy:          pxSvc,
		Databases:      dbService,
		Jobs:           jobsService,
		Backups:        backupService,
		Scheduler:      sched,
		Lifecycle:      lifecycle,
		Tiles:          tiles,
		Telemetry:      telemetry,
		Domains:        domains,
		Resources:      resources,
		Instances:      instances,
		Slices:         slices,
		Variables:      vars,
		Environments:   envSvc,
		Stacks:         stackSvc,
		Gate:           gate,
		Schedules:      scheduleSvc,
		Destinations:   destSvc,
		Storage:        storageSvc,
		Settings:       settingsSvc,
		NodeService:    nodeLifecycle,
		Containers:     service.NewContainerService(clus),
		ImageWatch:     imageWatch,
		Access:         accessSvc,
		Registries:     registrySvc,
		PREnvs:         prenvSvc,
		Members:        memberSvc,
		Orgs:           orgSvc,
		Audit:          auditSvc,
		Graph:          graphSvc,
		Connectors:     connectorSvc,
		Notifications:  notificationSvc,
		OrgConfig:      orgRunner,
		Deploys:        deploySvc,
		Releases:       releaseSvc,
		Plans:          planSvc,
		Metrics:        sampler,
		Forwards:       forwards,
		GitHub:         gh,
		Applier:        applier,
		Work:           work,
		DataDir:        envDataDir,
		RegistryPort:   registryPort,
		ACMEEmail:      config.GetEnvOrDefault("ACME_EMAIL", ""),
		Mail:           mailer,
		Nodes:          nodeSvc,
		Cluster:        clus,
		Mover:          mover,
		Version:        version,
		Admin:          adminService,
		Revoke:         revokeSvc,
	})

	log.Info("starting server", "port", envPort, "devMode", envDevMode, "version", version)
	err = srv.Start()
	hub.Close()
	if err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func writeOpenAPISpec(path string) error {
	a := v1.New(nil, nil, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{})
	a.Register(echo.New().Group("/api/v1")) // throwaway group; only the spec matters

	raw, err := a.SpecJSON()
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}
