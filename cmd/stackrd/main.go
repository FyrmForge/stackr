package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
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
	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/api"
	v1 "github.com/FyrmForge/stackr/internal/stackrd/handlers/api/v1"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
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

	// Base URL (cookie domain & CORS).
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

	// One-shot upgrade: legacy name:/path mounts become volume tiles.
	if err := envops.ConvertLegacyMounts(context.Background(), store); err != nil {
		log.Error("volume-tile conversion failed", "error", err)
	}

	// One-shot upgrade: project existing env blobs into variable rows, which is
	// what the resolver reads. Without it an upgraded install deploys apps with
	// an empty environment.
	if err := envops.BackfillVariables(context.Background(), store); err != nil {
		log.Error("variable backfill failed", "error", err)
	}

	cookieSecure := cookieSecureFor(envDevMode, envTLSOff)

	// Sessions.
	sessionManager := auth.NewSessionManager(store,
		auth.WithCookieSecure(cookieSecure),
		auth.WithCookieDomain(baseDomain),
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
	// An install upgraded across 009 has environments with no overlay at all,
	// because that migration added the column empty and only a deploy ever
	// fills it. Give them one now rather than at whatever point somebody next
	// redeploys.
	netpool.Backfill(context.Background(), store, rt)
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
	go func() {
		if _, err := registry.EnsureManaged(context.Background(), store, rt, registrySigner, envDataDir, registryPort, baseOrigin); err != nil {
			log.Error("managed registry start failed", "error", err)
		}
	}()
	// A restart dropped every forward websocket, so any relay still around is
	// a leftover; also undo the self-attachments the pre-relay design made.
	rt.SweepForwardState(context.Background())

	// WebSocket hub. Clients join rooms ({"action":"join","room":"..."}) and
	// get data-free "changed" events pushed by the notifier (live UI updates).
	var hub *websocket.Hub
	hub = websocket.NewHub(websocket.WithLogger(log), websocket.WithOnMessage(func(c *websocket.Client, msg []byte) {
		var m struct {
			Action string `json:"action"`
			Room   string `json:"room"`
		}
		if json.Unmarshal(msg, &m) != nil {
			return
		}
		switch m.Action {
		case "join":
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
	engine := deploy.NewEngine(store, rt, clus, streamHub, envDataDir, notifier)

	// GitHub connectors: private-repo clone auth, ghcr pulls, PR feedback.
	gh := githubapp.New(store, baseOrigin)
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
		// the preview-domain forwardAuth. The default matches the alias the
		// deploy script gives the stackr container.
		config.GetEnvOrDefault("PANEL_INTERNAL_URL", fmt.Sprintf("http://stkr-panel:%d", envPort)),
		// The panel's own public hostname, so Traefik can route to it without
		// anybody configuring a domain first.
		baseDomain,
		envTLSOff,
	)
	go func() {
		if err := px.EnsureTraefik(context.Background()); err != nil {
			log.Error("traefik startup failed", "error", err)
		}
		// Dynamic configs are renders of DB state, regenerate them so a
		// wiped/restored data dir heals itself.
		if err := px.Resync(context.Background()); err != nil {
			log.Error("proxy resync failed", "error", err)
		}
	}()

	// Metrics sampler + nightly cleanup.
	sampler := metrics.NewSampler(store, clus, notifier)
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
			if v, _ := store.GetSetting(context.Background(), "cleanup_enabled"); v == "1" {
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

	dbService := managedtiles.NewService(clus, store)

	// Scheduled jobs (cron commands for apps).
	jobsService := jobs.NewService(store, clus, notifier)
	// Function tiles with the on-deploy trigger fire once their own build
	// lands, chained onto the GitHub hook above rather than replacing it.
	prevFinish := engine.OnFinish
	engine.OnFinish = func(tile *repo.Tile, d *repo.Deployment) {
		if prevFinish != nil {
			prevFinish(tile, d)
		}
		if tile.Kind == "function" && tile.RunOnDeploy && d.Status == "done" {
			go jobsService.RunApp(context.Background(), tile.ID, jobs.TriggerDeploy, "")
		}
		// Baseline the pulled digest so the image watcher compares against
		// what actually runs (git builds carry no registry digest).
		if d.Status == "done" && tile.SourceType == "image" {
			if dg, err := clus.LocalDigest(context.Background(), tile.ImageRef); err == nil && dg != "" {
				_ = store.SetTileImageDigest(context.Background(), tile.ID, dg)
			}
		}
	}
	if err := jobsService.LoadSchedules(context.Background()); err != nil {
		log.Error("loading job schedules failed", "error", err)
	}

	// Registry watcher (per-tile update_policy) on a 1-minute janitor tick;
	// the real cadence is the image_check_interval setting, due-checked
	// in-task because janitor schedules are fixed at AddTask time.
	watcher := &imagewatch.Watcher{Store: store, Notifier: notifier, Engine: engine,
		DB: dbService, Cluster: clus, GH: gh, Client: &imagewatch.Client{}}
	jan := janitor.New(janitor.WithTimeout(2*time.Minute), janitor.WithLogger(log))
	jan.AddTask("@every 1m", watcher)
	// CI gate: releases (or fails) push deploys parked behind wait_for_ci.
	jan.AddTask("@every 1m", &cigate.Gate{Store: store, Engine: engine, GH: gh, Notifier: notifier})
	if err := jan.Start(context.Background()); err != nil {
		log.Error("janitor start failed", "error", err)
	}

	// Backups: dumps, volume archives, and the panel's own database. Holds the
	// live *sqlx.DB because VACUUM INTO is the only consistent way to copy the
	// file while stackr is serving.
	backupService := backup.NewService(store, dbService, clus, database, envDataDir, version)
	if err := backupService.LoadSchedules(context.Background()); err != nil {
		log.Error("loading backup schedules failed", "error", err)
	}

	// Handed to everything that touches a container or a volume: both live on
	// one node's disk, and the manager's own socket answers only for itself.
	// Set here rather than in the constructors because the dispatcher needs
	// the runtime, which these were built from.
	// A swarm with more than one node runs Ensure on every boot, not only
	// when the service is missing. It is the upgrade path: the new panel
	// pushes its own image and updates the agent service to that digest, so
	// the agents roll to the same build (docs/plans/30-docker-swarm.md, step
	// 7). Skipping it while the service exists leaves every node on the old
	// agent, which the panel then refuses on the version header.
	go func() {
		ctx := context.Background()
		ns, err := rt.ListNodes(ctx)
		if err != nil || len(ns) < 2 {
			return
		}
		if err := agent.Ensure(ctx, store, rt, envDataDir); err != nil {
			log.Error("node agent service", "error", err)
		}
	}()
	nodeSvc := &nodes.Service{Store: store, RT: rt}
	// Sync writes the manager's own swarm id onto its servers row. Nothing
	// else does, and every screen keyed on servers.node_id (tile placement,
	// /servers/local, volume delete) reads the manager as not joined until
	// someone happens to open /servers.
	if _, err := nodeSvc.Sync(context.Background()); err != nil {
		log.Error("node sync", "error", err)
	}
	mover := volmove.New(store, clus, engine, dbService)
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
	applier := stackconf.Applier{
		Planner: stackconf.Planner{Store: store, Src: gh},
		Ops:     envops.Ops{Store: store, RT: rt, Cluster: clus, PX: px, DBs: dbService},
		DBs:     dbService,
		Engine:  engine,
		Jobs:    jobsService,
		Backups: backupService,
	}
	orgRunner := &orgconf.Runner{Store: store, Src: gh, Stacks: applier.Planner, Applier: applier}

	// One durable runner for long work (docs/plans/33-workqueue.md). Applies
	// go on it first: they were the only long job with no queue at all, and
	// they ran on the HTTP request's context, so a browser that stopped
	// waiting killed the apply half way through and the failure was never
	// recorded.
	work := workqueue.New(store)
	stackconf.RegisterApply(work, applier)
	engine.WithWork(work)
	work.Start(context.Background())
	// Optional: with no provider configured this is nil and invites fall back
	// to a link the inviter passes on by hand.
	mailer := mail.FromEnv()
	if mailer.Enabled() {
		log.Info("outbound mail configured")
	}

	api.RegisterRoutes(srv, &api.Deps{
		Store:          store,
		Cluster:        clus,
		Engine:         engine,
		Runtime:        rt,
		Proxy:          px,
		Jobs:           jobsService,
		Backups:        backupService,
		Forwards:       forwards,
		Notifier:       notifier,
		Applier:        applier,
		Work:           work,
		Nodes:          nodeSvc,
		DataDir:        envDataDir,
		RegistrySigner: registrySigner,
	})

	web.RegisterRoutes(srv, &web.Deps{
		Store:          store,
		BaseURL:        baseOrigin,
		CookieDomain:   baseDomain,
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
		Proxy:          px,
		Databases:      dbService,
		Jobs:           jobsService,
		Backups:        backupService,
		Metrics:        sampler,
		Forwards:       forwards,
		GitHub:         gh,
		Applier:        applier,
		Work:           work,
		DataDir:        envDataDir,
		RegistryPort:   registryPort,
		ACMEEmail:      config.GetEnvOrDefault("ACME_EMAIL", ""),
		OrgConfig:      orgRunner,
		Mail:           mailer,
		Nodes:          nodeSvc,
		Cluster:        clus,
		Mover:          mover,
		Version:        version,
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
