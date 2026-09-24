package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"github.com/FyrmForge/hamr/pkg/auth"
	"github.com/FyrmForge/hamr/pkg/config"
	db "github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/hamr/pkg/storage"
	"github.com/FyrmForge/hamr/pkg/email"
	"github.com/FyrmForge/hamr/pkg/emailmock"
	appdb "github.com/FyrmForge/stackr/internal/db"
	"github.com/FyrmForge/stackr/internal/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/components"
)

// version is set at build time via ldflags.
var version = "dev"

var (
	envPort          = config.GetEnvOrDefaultInt("PORT", 8080)
	// DEV_MODE defaults to false (fail closed in prod). Local dev sets
	// DEV_MODE=true via .env so the scaffolded `.env` ships with it set
	// explicitly. This makes the STRIPE_MOCK production guard actually
	// guard — a leftover STRIPE_MOCK=true in a prod deploy without
	// DEV_MODE explicitly set would otherwise slip through.
	envDevMode       = config.GetEnvOrDefaultBool("DEV_MODE", false)
	envBaseURL       = config.GetEnvOrDefault("BASE_URL", "")
	envDatabasePath  = config.GetEnvOrDefault("DATABASE_PATH", "./data/stackr.db")
	envStaticBaseURL = config.GetEnvOrDefault("STATIC_BASE_URL", "/static")
	envStoragePath   = config.GetEnvOrDefault("STORAGE_PATH", "./uploads")
	envEmailMock   = config.GetEnvOrDefaultBool("EMAIL_MOCK", false)
	envHamrDevURL  = config.GetEnvOrDefault("HAMR_DEV_URL", "http://localhost:3000")
	envTrustedProxies = config.GetEnvCSV("TRUSTED_PROXIES")
)

func main() {
	generateFlag := flag.Bool("generate", false, "generate static pages and exit")
	flag.Parse()

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
		// "cloudflare" trusts Cloudflare's edge ranges, refreshed every 24h.
		server.WithTrustedProxies(envTrustedProxies...),
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

	// Static page generation — no heavy deps needed.
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
	defer cancel()
	database, err := db.ConnectContext(connectCtx, envDatabasePath)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}

	// Run migrations at startup.
	if err := db.Migrate(database, appdb.MigrateConfig()); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
	log.Info("migrations completed")
	store := sqlite.NewStore(database)

	// Sessions.
	sessionManager := auth.NewSessionManager(store,
		auth.WithCookieSecure(!envDevMode),
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

	// Email sender. In dev (EMAIL_MOCK=true), ships messages to the hamr dev
	// inbox at HAMR_DEV_URL/__hamr/mail. Swap for a real provider adapter in
	// production.
	var emailSender email.Sender
	if envEmailMock {
		emailSender = emailmock.New(envHamrDevURL)
		log.Info("email mock enabled", "inbox", envHamrDevURL+"/__hamr/mail")
	}

	api.RegisterRoutes(srv, &api.Deps{
		Store: store,
	})

	web.RegisterRoutes(srv, &web.Deps{
		Store:         store,
		BaseURL:       baseOrigin,
		StaticBaseURL: envStaticBaseURL,
		DevMode:       envDevMode,
		SessionManager: sessionManager,
		AuthService: authService,
		FileStorage: fileStorage,
		EmailSender: emailSender,
	})

	log.Info("starting server", "port", envPort, "devMode", envDevMode)
	if err := srv.Start(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
