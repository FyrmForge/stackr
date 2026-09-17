package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/netaddr"
	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// seededKey marks the installer's answers as copied in. Once only: after the
// first boot the panel owns them, and a value the operator removed must not
// come back on the next restart.
const seededKey = "install_seeded"

// seedInstall copies install.sh's answers into the store on first boot
// (docs/plans/48-install-domains-and-basic-auth.md): ROOT_DOMAIN becomes the
// instance domain resource, TRUST_CLOUDFLARE and TRUSTED_PROXY_CIDRS the
// Proxy page's settings. Runs before Traefik starts, which reads the latter.
//
// refreshCF fetches Cloudflare's ranges. On failure the box stays off rather
// than on with nothing trusted, the Proxy page's own rule.
func seedInstall(ctx context.Context, store repo.Store, rootDomain, trustCF, cidrs string, refreshCF func(context.Context) error) error {
	if done, err := store.GetSetting(ctx, seededKey); err != nil || done == "1" {
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
		if sv != nil && !envops.HostTaken(all, root) {
			if err := store.CreateDomainResource(ctx, &repo.DomainResource{
				ID: uuid.New().String(), Level: "instance", OwnerID: sv.ID, Host: root, CreatedAt: time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
	}
	var lines []string
	for _, c := range strings.Split(cidrs, ",") {
		if strings.TrimSpace(c) == "" {
			continue
		}
		cidr, err := netaddr.ParseTrusted(c)
		if err != nil {
			slog.Warn("TRUSTED_PROXY_CIDRS: skipping", "error", err)
			continue
		}
		lines = append(lines, cidr)
	}
	if len(lines) > 0 {
		if err := store.SetSetting(ctx, "trusted_proxies", strings.Join(lines, "\n")); err != nil {
			return err
		}
	}
	if trustCF == "1" {
		if err := refreshCF(ctx); err != nil {
			slog.Warn("could not fetch Cloudflare ranges, leaving Trust Cloudflare off; turn it on from the Proxy page", "error", err)
		} else if err := store.SetSetting(ctx, "trust_cloudflare", "1"); err != nil {
			return err
		}
	}
	return store.SetSetting(ctx, seededKey, "1")
}
