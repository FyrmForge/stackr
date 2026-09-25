package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/caddy-dns/cloudflare"
	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/standard"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"

	"github.com/FyrmForge/stackr/internal/service"
)

// runProxy is `stackrd proxy`: Caddy as a library, a config sink with no
// Docker socket. Caddy's data and config dirs come from XDG_DATA_HOME and
// XDG_CONFIG_HOME, which the installer points at the proxy's named volume,
// so a restart resumes the autosaved routes and keeps its certificates.
// stackrd re-pushes the full config whenever it sees this container start.
func runProxy() error {
	if err := startProxy(service.ProxyAdmin); err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	return caddy.Stop()
}

// startProxy loads the autosaved config, or a bare admin listener when there
// is none or it no longer loads (stackrd's next push brings the routes).
func startProxy(admin string) error {
	bare := fmt.Appendf(nil, `{"admin":{"listen":%q}}`, admin)
	cfg, err := os.ReadFile(caddy.ConfigAutosavePath)
	if err == nil {
		if err = caddy.Load(cfg, true); err == nil {
			return nil
		}
	}
	if err2 := caddy.Load(bare, true); err2 != nil {
		return errors.Join(err, err2)
	}
	return nil
}
