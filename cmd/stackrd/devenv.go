package main

import (
	"bufio"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service"
)

// devSettings reads <dataDir>/dev.env once at start: KEY=VALUE lines, #
// comments and blank lines ignored. A missing file changes nothing. It is
// the playground's way to point stackrd at a private ACME CA, a fake
// Namecheap and a fake GitHub; production has no such file. Keys: ACME_CA,
// ACME_CA_ROOT, NAMECHEAP_API, GITHUB_URL, GITHUB_AVATARS, GITHUB_CA, EXTRA_CA_DIR.
func devSettings(log *slog.Logger, dataDir string, cfg *service.Config) {
	f, err := os.Open(filepath.Join(dataDir, "dev.env"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("dev.env unreadable", "err", err)
		}
		return
	}
	defer func() { _ = f.Close() }()
	var active []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "ACME_CA":
			cfg.ACMECA = v
		case "ACME_CA_ROOT": // a file name in the proxy admin dir, same on every node
			cfg.ACMECARoot = path.Join(installspec.ProxyAdminDir, v)
		case "NAMECHEAP_API":
			cfg.NamecheapAPI = v
		case "GITHUB_URL":
			cfg.GitHubURL = v
		case "GITHUB_AVATARS":
			cfg.GitHubAvatars = v
		case "GITHUB_CA": // a full path inside the panel container
			cfg.GitHubCA = v
		case "EXTRA_CA_DIR": // a dir of PEM roots inside the panel container, read once by Go at first TLS use
			if err := os.Setenv("SSL_CERT_DIR", "/etc/ssl/certs:"+v); err != nil {
				log.Warn("dev.env: EXTRA_CA_DIR not applied", "err", err)
				continue
			}
		default:
			log.Warn("dev.env: unknown key ignored", "key", k)
			continue
		}
		active = append(active, k)
	}
	if len(active) > 0 {
		log.Info("dev.env active", "keys", strings.Join(active, ","))
	}
}
