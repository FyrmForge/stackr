package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// seedInstall copies install.sh's answers into the store on first boot
// (docs/plans/48-install-domains-and-basic-auth.md): ROOT_DOMAIN becomes the
// instance domain resource, TRUST_CLOUDFLARE and TRUSTED_PROXY_CIDRS the
// Proxy page's settings. Runs before Traefik starts, which reads the latter.
//
// refreshCF fetches Cloudflare's ranges. On failure the box stays off rather
// than on with nothing trusted, the Proxy page's own rule.
func seedInstall(ctx context.Context, store repo.Store, set *service.SettingsService,
	rootDomain, trustCF, cidrs string, refreshCF func(context.Context) error) error {
	if done, err := set.Value(ctx, settings.KeyInstallSeeded); err != nil || done == "1" {
		return err
	}
	if root := strings.ToLower(strings.TrimSpace(rootDomain)); root != "" {
		all, err := store.ListDomainResources(ctx)
		if err != nil {
			return err
		}
		sv, err := store.GetServer(ctx, "local")
		if err != nil {
			return err
		}
		if sv != nil && !service.HostTaken(all, root) {
			if err := store.CreateDomainResource(ctx, &repo.DomainResource{
				ID: uuid.New().String(), Level: "instance", OwnerID: sv.ID, Host: root, CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
	}
	// One policy with the panel and the installer: a bad line refuses the
	// list. Skipping it used to leave a trusted proxy that is not trusted,
	// which shows up later as every client IP being the proxy's.
	lines, err := svcproxy.ParseTrustedList(cidrs)
	if err != nil {
		return fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err)
	}
	if len(lines) > 0 {
		if err := set.SetValue(ctx, settings.KeyTrustedProxies, strings.Join(lines, "\n")); err != nil {
			return err
		}
	}
	if trustCF == "1" {
		if err := refreshCF(ctx); err != nil {
			slog.Warn("could not fetch Cloudflare ranges, leaving Trust Cloudflare off; turn it on from the Proxy page", "error", err)
		} else if err := set.SetValue(ctx, settings.KeyTrustCF, "1"); err != nil {
			return err
		}
	}
	return set.SetValue(ctx, settings.KeyInstallSeeded, "1")
}
