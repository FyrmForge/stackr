package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/joho/godotenv/autoload"

	"github.com/FyrmForge/hamr/pkg/config"
	"github.com/FyrmForge/hamr/pkg/email"
	"github.com/FyrmForge/hamr/pkg/emailmock"
	"github.com/FyrmForge/hamr/pkg/logging"
	"github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/components"
)

// version is set at build time via ldflags.
var version = "dev"

var (
	envPort = config.GetEnvOrDefaultInt("PORT", 8080)
	// DEV_MODE defaults to false (fail closed in prod). Local dev sets
	// DEV_MODE=true via .env so the scaffolded `.env` ships with it set
	// explicitly. This makes the STRIPE_MOCK production guard actually
	// guard — a leftover STRIPE_MOCK=true in a prod deploy without
	// DEV_MODE explicitly set would otherwise slip through.
	envDevMode      = config.GetEnvOrDefaultBool("DEV_MODE", false)
	envBaseURL      = config.GetEnvOrDefault("BASE_URL", "")
	envDataDir      = config.GetEnvOrDefault("DATA_DIR", "./data")
	envDatabasePath = config.GetEnvOrDefault("DATABASE_PATH", "")
	// STACKR_MASTER_KEY encrypts secrets at rest: 64 hex chars. stackrd
	// never makes one up; the installer writes it (DECIDE 14).
	envMasterKey      = config.GetEnvOrDefault("STACKR_MASTER_KEY", "")
	envStaticBaseURL  = config.GetEnvOrDefault("STATIC_BASE_URL", "/static")
	envEmailMock      = config.GetEnvOrDefaultBool("EMAIL_MOCK", false)
	envHamrDevURL     = config.GetEnvOrDefault("HAMR_DEV_URL", "http://localhost:3000")
	envTrustedProxies = config.GetEnvCSV("TRUSTED_PROXIES")
)

func main() {
	generateFlag := flag.Bool("generate", false, "generate static pages and exit")
	versionFlag := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println(version)
		return
	}

	log := logging.New(!envDevMode)
	slog.SetDefault(log)

	if err := run(log, *generateFlag); err != nil {
		log.Error("stackrd stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, generate bool) error {
	components.StaticBaseURL = envStaticBaseURL

	// Base URL (cookie domain & CORS).
	baseOrigin, baseDomain, err := config.ParseBaseURL(envBaseURL)
	if err != nil {
		return fmt.Errorf("invalid BASE_URL: %w", err)
	}
	components.BaseURL = baseOrigin

	// Server.
	srv, err := server.New(
		server.WithPort(envPort),
		server.WithDevMode(envDevMode),
		server.WithStaticDir("ui/static"),
		server.WithStaticDistDir("ui/dist"),
		server.WithGeneratedDir("generated"),
		// TRUSTED_PROXIES: comma-separated CIDRs of upstream proxies/load
		// balancers allowed to set X-Forwarded-For (drives client-IP detection
		// and the rate-limit key). Empty/unset ignores X-Forwarded-For so a
		// direct client can't spoof its IP; set it to your LB ranges behind one.
		// "cloudflare" trusts Cloudflare's edge ranges, refreshed every 24h.
		server.WithTrustedProxies(envTrustedProxies...),
	)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	if baseOrigin != "" {
		srv.Echo().Use(middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins:     []string{baseOrigin},
			AllowCredentials: true,
		}))
	}

	// Static page generation — no heavy deps needed.
	web.RegisterStaticPages(srv)
	if generate {
		if err := srv.GenerateStatic("generated"); err != nil {
			return fmt.Errorf("generate static pages: %w", err)
		}
		return nil
	}

	// The service tree: store, migrations, Docker, leaves and flows.
	svc, err := service.New(service.Config{
		DataDir:      envDataDir,
		DBPath:       envDatabasePath,
		SecretsKey:   envMasterKey,
		CookieSecure: !envDevMode,
		CookieDomain: baseDomain,
	})
	if err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// Email sender. In dev (EMAIL_MOCK=true), ships messages to the hamr dev
	// inbox at HAMR_DEV_URL/__hamr/mail. Swap for a real provider adapter in
	// production.
	var emailSender email.Sender
	if envEmailMock {
		emailSender = emailmock.New(envHamrDevURL)
		log.Info("email mock enabled", "inbox", envHamrDevURL+"/__hamr/mail")
	}

	api.RegisterRoutes(srv, &api.Deps{
		Service: svc,
	})

	web.RegisterRoutes(srv, &web.Deps{
		Service:       svc,
		BaseURL:       baseOrigin,
		StaticBaseURL: envStaticBaseURL,
		DevMode:       envDevMode,
		EmailSender:   emailSender,
	})

	log.Info("starting server", "port", envPort, "devMode", envDevMode)
	return srv.Start()
}
